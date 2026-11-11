package importer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// IDsConfig holds the options of "update -ids".
type IDsConfig struct {
	MaxTraces int           // download at most this many missing traces per run
	MaxTime   time.Duration // or for at most this time
}

// DefaultIDsConfig returns the default options.
func DefaultIDsConfig() IDsConfig {
	return IDsConfig{MaxTraces: 1000, MaxTime: 10 * time.Minute}
}

// IDsStats is the result of one "update -ids" run.
type IDsStats struct {
	ListIDs     int   // ids in the list
	ListMax     int64 // highest id in the list
	Deleted     int
	Missing     int // ids in the list and not in the database
	Inserted    int
	Skipped     int // missing ids the website no longer serves, or without points
	Left        int // missing ids left for the next run
	Segments    int
	ExpireTiles int
	ExpireFile  string
}

// ReadIDList reads a list of trace ids, one per line, like
// gpx_ids/identifiable.txt on planet.openstreetmap.org. Lines that start
// with "#" and empty lines are ignored. Any other bad line is an error, so
// a broken list never deletes traces.
func ReadIDList(in io.Reader) ([]int64, error) {
	var ids []int64
	sc := bufio.NewScanner(in)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, err := strconv.ParseInt(line, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("id list line %d: bad id %q", n, line)
		}
		ids = append(ids, id)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading id list: %w", err)
	}
	if len(ids) == 0 {
		return nil, errors.New("id list is empty")
	}
	return ids, nil
}

// RunIDs syncs the database with the list of identifiable trace ids:
//   - id in the database and not in the list: the trace was deleted or is
//     no longer identifiable, delete it
//   - id in the list and not in the database: download it from the website
//
// Ids higher than the highest id in the list are kept: they are newer than
// the list and came from "update -rss". Missing traces are downloaded up to
// MaxTraces or MaxTime; the rest wait for the next run.
func RunIDs(ctx context.Context, conn *pgx.Conn, f *Fetcher, list []int64, cfg Config, ic IDsConfig) (IDsStats, error) {
	st := IDsStats{ListIDs: len(list)}
	start := time.Now()

	inList := make(map[int64]bool, len(list))
	for _, id := range list {
		inList[id] = true
		st.ListMax = max(st.ListMax, id)
	}
	// The grid tables are the ones import made.
	var err error
	if cfg.LowZooms, err = LowZooms(ctx, conn); err != nil {
		return st, err
	}
	if err := checkGrid(ctx, conn, cfg.LowZooms); err != nil {
		return st, err
	}
	dbIDs, err := loadIDs(ctx, conn)
	if err != nil {
		return st, err
	}
	if len(dbIDs) == 0 {
		return st, errors.New("the database is empty: load a dump with import first")
	}

	var gone []int64
	for id := range dbIDs {
		if id <= st.ListMax && !inList[id] {
			gone = append(gone, id)
		}
	}
	if cfg.MaxDeleteRatio > 0 && float64(len(gone)) > cfg.MaxDeleteRatio*float64(len(dbIDs)) {
		return st, fmt.Errorf("the list would delete %d of %d traces; check the list or raise -max-delete-ratio",
			len(gone), len(dbIDs))
	}
	var missing []int64
	for id := range inList {
		if _, ok := dbIDs[id]; !ok {
			missing = append(missing, id)
		}
	}
	slices.Sort(missing)
	st.Missing = len(missing)
	cfg.Log.Printf("id list: %d ids up to %d; database: %d traces; %d to delete, %d missing",
		len(list), st.ListMax, len(dbIDs), len(gone), len(missing))

	var exp *Expire
	if cfg.ExpireDir != "" {
		if exp, err = NewExpire(cfg.ExpireDir, cfg.ExpireMinZoom, cfg.ExpireMaxZoom, time.Now()); err != nil {
			return st, err
		}
	}

	if st.Deleted, err = deleteTraces(ctx, conn, gone, exp, cfg); err != nil {
		return st, err
	}

	split := cfg.split()
	b := &batch{}
	var runErr error
	done := 0
	for _, id := range missing {
		if ic.MaxTraces > 0 && done >= ic.MaxTraces {
			break
		}
		if ic.MaxTime > 0 && time.Since(start) >= ic.MaxTime {
			cfg.Log.Printf("time limit reached")
			break
		}
		segs, err := f.Trace(ctx, id)
		if errors.Is(err, ErrNotFound) {
			// Deleted or changed after the list was made.
			st.Skipped++
			done++
			continue
		}
		if err != nil {
			// Network or server error: stop, the next run tries again.
			runErr = err
			break
		}
		done++
		if countPoints(segs) == 0 {
			st.Skipped++
			continue
		}
		st.Segments += b.addNew(&Trace{ID: id, Segments: segs}, split, exp)
		st.Inserted++
		if b.count >= cfg.BatchSize {
			if err := b.flush(ctx, conn, exp); err != nil {
				return st, err
			}
		}
	}
	if err := b.flush(ctx, conn, exp); err != nil {
		return st, err
	}
	st.Left = len(missing) - done
	st.ExpireTiles = exp.Count()
	if st.ExpireFile, err = exp.Close(); err != nil {
		return st, err
	}
	return st, runErr
}
