package importer

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// RSSConfig holds the options of "update --rss".
type RSSConfig struct {
	MaxTraces int           // stop after checking this many ids
	MaxTime   time.Duration // or after this time, like gpx-updater
	// First run only: start after this id. 0: the highest id in the
	// database. -1: the highest id in the RSS feed now, so only the traces
	// uploaded from now on are loaded (for a database loaded from an old
	// dump, when the traces in between come from the id list).
	StartID int64
}

// DefaultRSSConfig returns the default options.
func DefaultRSSConfig() RSSConfig {
	return RSSConfig{MaxTraces: 1000, MaxTime: 10 * time.Minute}
}

// RSSStats is the result of one "update --rss" run.
type RSSStats struct {
	FromID, ToID int64 // ids checked in this run
	Checked      int
	Inserted     int
	Skipped      int // not public, deleted, already loaded or without points
	Segments     int
	ExpireTiles  int
	ExpireFile   string
}

var traceLink = regexp.MustCompile(`/traces/([0-9]+)\b`)

// MaxRSSID returns the highest trace id in the website RSS feed.
// The feed only lists the last 20 public traces, so it is used only to
// know how far to go. The ids in between are checked one by one.
func MaxRSSID(in io.Reader) (int64, error) {
	var feed struct {
		Items []struct {
			Link string `xml:"link"`
		} `xml:"channel>item"`
	}
	if err := xml.NewDecoder(in).Decode(&feed); err != nil {
		return 0, fmt.Errorf("reading RSS: %w", err)
	}
	var max int64
	for _, it := range feed.Items {
		m := traceLink.FindStringSubmatch(it.Link)
		if m == nil {
			continue
		}
		if id, err := strconv.ParseInt(m[1], 10, 64); err == nil && id > max {
			max = id
		}
	}
	if max == 0 {
		return 0, errors.New("RSS feed has no trace links")
	}
	return max, nil
}

// RunRSS loads the new traces, like gpx-updater does today:
//  1. read the RSS feed to get the highest trace id
//  2. check every id after the last one checked, up to that id
//  3. download the public ones and insert them
//
// It only inserts. Deleted traces are removed later by "import" or
// "update --ids". The last checked id is kept in rss_state, so a run that
// stops can start again where it was.
func RunRSS(ctx context.Context, conn *pgx.Conn, f *Fetcher, cfg Config, rc RSSConfig) (RSSStats, error) {
	var st RSSStats
	start := time.Now()

	body, err := f.get(ctx, "/traces/rss")
	if err != nil {
		return st, err
	}
	max, err := MaxRSSID(body)
	body.Close()
	if err != nil {
		return st, err
	}
	last, err := rssStart(ctx, conn, rc.StartID, max)
	if err != nil {
		return st, err
	}
	st.FromID, st.ToID = last+1, last
	if max <= last {
		cfg.Log.Printf("no new traces (last checked %d, RSS %d)", last, max)
		return st, nil
	}
	to := min(max, last+int64(rc.MaxTraces))
	cfg.Log.Printf("checking traces %d to %d (RSS %d)", last+1, to, max)

	// The grid tables are the ones import made.
	if cfg.LowZooms, err = LowZooms(ctx, conn); err != nil {
		return st, err
	}
	// Ids already loaded, for example by a newer dump.
	loaded, err := loadIDRange(ctx, conn, last, to)
	if err != nil {
		return st, err
	}

	var exp *Expire
	if cfg.ExpireDir != "" {
		if exp, err = NewExpire(cfg.ExpireDir, cfg.ExpireMinZoom, cfg.ExpireMaxZoom, time.Now()); err != nil {
			return st, err
		}
	}
	split := cfg.split()
	b := &batch{}
	done := last // all ids up to here are checked

	// save writes the batch and then the state, so an id is never marked
	// as checked before its trace is in the database.
	save := func() error {
		if err := b.flush(ctx, conn, exp); err != nil {
			return err
		}
		if done == last {
			return nil
		}
		_, err := conn.Exec(ctx, `INSERT INTO rss_state (id, last_id) VALUES (true, $1)
			ON CONFLICT (id) DO UPDATE SET last_id = EXCLUDED.last_id`, done)
		return err
	}

	var runErr error
	for id := last + 1; id <= to; id++ {
		if rc.MaxTime > 0 && time.Since(start) >= rc.MaxTime {
			cfg.Log.Printf("time limit reached at trace %d", id)
			break
		}
		st.Checked++
		if loaded[id] {
			st.Skipped++
			done = id
			continue
		}
		segs, err := f.Trace(ctx, id)
		if errors.Is(err, ErrNotFound) {
			st.Skipped++
			done = id
			continue
		}
		if err != nil {
			// Network or server error: stop here and try this id next run.
			runErr = err
			break
		}
		if countPoints(segs) == 0 {
			st.Skipped++
			done = id
			continue
		}
		// The RSS mode has no name, description or tags: the website only
		// gives them through the API, with a login. The next dump adds them.
		st.Segments += b.addNew(&Trace{ID: id, Segments: segs}, split, exp)
		st.Inserted++
		done = id
		if b.count >= cfg.BatchSize {
			if err := save(); err != nil {
				return st, err
			}
			last = done
		}
	}
	if err := save(); err != nil {
		return st, err
	}
	st.ToID = done
	st.ExpireTiles = exp.Count()
	if st.ExpireFile, err = exp.Close(); err != nil {
		return st, err
	}
	return st, runErr
}

// rssStart returns the last checked id. On the first run it is startID, or
// the highest id in the database when startID is 0.
func rssStart(ctx context.Context, conn *pgx.Conn, startID, rssMax int64) (int64, error) {
	var last int64
	err := conn.QueryRow(ctx, "SELECT last_id FROM rss_state").Scan(&last)
	if err == nil {
		return last, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	switch {
	case startID > 0:
		last = startID
	case startID < 0:
		last = rssMax
	default:
		var max *int64
		if err := conn.QueryRow(ctx, "SELECT max(gpx_id) FROM traces").Scan(&max); err != nil {
			return 0, err
		}
		if max == nil {
			return 0, errors.New("the database is empty: load a dump with import first, or set -start-id")
		}
		last = *max
	}
	// Keep it, so the next run goes on from here even if nothing is loaded now.
	_, err = conn.Exec(ctx, "INSERT INTO rss_state (id, last_id) VALUES (true, $1)", last)
	return last, err
}

func loadIDRange(ctx context.Context, conn *pgx.Conn, after, to int64) (map[int64]bool, error) {
	rows, err := conn.Query(ctx, "SELECT gpx_id FROM traces WHERE gpx_id > $1 AND gpx_id <= $2", after, to)
	if err != nil {
		return nil, err
	}
	ids := map[int64]bool{}
	var id int64
	_, err = pgx.ForEachRow(rows, []any{&id}, func() error {
		ids[id] = true
		return nil
	})
	return ids, err
}
