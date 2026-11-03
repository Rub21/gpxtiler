// Package importer loads the planet-dump-ng GPX dump into PostGIS.
//
// Each run syncs the database with the dump:
//   - new id: insert the trace and its geometry
//   - known id: update name, description and tags (geometry never changes)
//   - id not in the dump: the trace was deleted or made private, delete it
package importer

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Config holds the import options.
type Config struct {
	BatchSize      int     // traces per transaction
	MaxPoints      int     // cut lines after this many points
	MaxLength      float64 // or after this many meters
	LowZoom        int     // trace_segments_low is simplified for this zoom
	ExpireDir      string  // "" disables expire files
	ExpireZoom     int
	MaxDeleteRatio float64 // stop if the run would delete more than this share of traces
	Log            *log.Logger
}

// DefaultConfig returns the default options.
func DefaultConfig() Config {
	return Config{
		BatchSize:      1000,
		MaxPoints:      1000,
		MaxLength:      50000,
		LowZoom:        8,
		ExpireZoom:     14,
		MaxDeleteRatio: 0.5,
		Log:            log.Default(),
	}
}

// lowTolerance is the size of one pixel at LowZoom, for 256 px tiles.
func (c Config) lowTolerance() float64 {
	return 2 * mercMax / (256 * float64(int(1)<<c.LowZoom))
}

// Stats is the result of one run.
type Stats struct {
	DumpTime    time.Time
	FirstLoad   bool
	Inserted    int
	Updated     int
	Deleted     int
	Segments    int
	ExpireTiles int
	ExpireFile  string
}

// Run reads the dump from in and syncs the tables. The tables must exist.
// It can run again after a failure: each batch is one transaction and
// dump_state is written only at the end.
func Run(ctx context.Context, conn *pgx.Conn, in io.Reader, cfg Config) (Stats, error) {
	var st Stats
	rd, err := NewReader(in)
	if err != nil {
		return st, err
	}
	st.DumpTime = rd.DumpTime

	var last time.Time
	err = conn.QueryRow(ctx, "SELECT timestamp FROM dump_state").Scan(&last)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		st.FirstLoad = true
	case err != nil:
		return st, err
	case rd.DumpTime.Before(last):
		return st, fmt.Errorf("dump time %s is older than the loaded dump %s",
			rd.DumpTime.Format(time.RFC3339), last.Format(time.RFC3339))
	}

	// id -> seen in this dump
	ids, err := loadIDs(ctx, conn)
	if err != nil {
		return st, err
	}
	existing := len(ids)
	cfg.Log.Printf("dump time %s, %d traces in the database", rd.DumpTime.Format(time.RFC3339), existing)

	// The first load writes no expire file, the first render is complete.
	var exp *Expire
	if !st.FirstLoad && cfg.ExpireDir != "" {
		if exp, err = NewExpire(cfg.ExpireDir, cfg.ExpireZoom, time.Now()); err != nil {
			return st, err
		}
	}

	split := SplitConfig{MaxPoints: cfg.MaxPoints, MaxLength: cfg.MaxLength, LowTolerance: cfg.lowTolerance()}
	skipGeom := func(id int64) bool { _, ok := ids[id]; return ok }
	b := &batch{}
	total := 0
	for {
		t, err := rd.Next(skipGeom)
		if err == io.EOF {
			break
		}
		if err != nil {
			return st, fmt.Errorf("reading dump: %w", err)
		}
		total++
		seen, known := ids[t.ID]
		switch {
		case seen:
			cfg.Log.Printf("trace %d is twice in the dump, using the first one", t.ID)
			continue
		case known:
			b.addUpdate(t)
			st.Updated++
		default:
			st.Segments += b.addNew(t, split, exp)
			st.Inserted++
		}
		ids[t.ID] = true
		if b.count >= cfg.BatchSize {
			if err := b.flush(ctx, conn, exp); err != nil {
				return st, err
			}
		}
		if total%100000 == 0 {
			cfg.Log.Printf("%d traces read (%d new, %d updated)", total, st.Inserted, st.Updated)
		}
	}
	if err := b.flush(ctx, conn, exp); err != nil {
		return st, err
	}

	var gone []int64
	for id, seen := range ids {
		if !seen {
			gone = append(gone, id)
		}
	}
	if existing > 0 && cfg.MaxDeleteRatio > 0 && float64(len(gone)) > cfg.MaxDeleteRatio*float64(existing) {
		return st, fmt.Errorf("the dump would delete %d of %d traces; check the dump or raise -max-delete-ratio",
			len(gone), existing)
	}
	if st.Deleted, err = deleteTraces(ctx, conn, gone, exp); err != nil {
		return st, err
	}

	_, err = conn.Exec(ctx, `INSERT INTO dump_state (id, timestamp) VALUES (true, $1)
		ON CONFLICT (id) DO UPDATE SET timestamp = EXCLUDED.timestamp`, rd.DumpTime)
	if err != nil {
		return st, err
	}
	st.ExpireTiles = exp.Count()
	st.ExpireFile, err = exp.Close()
	return st, err
}

func loadIDs(ctx context.Context, conn *pgx.Conn) (map[int64]bool, error) {
	rows, err := conn.Query(ctx, "SELECT gpx_id FROM traces")
	if err != nil {
		return nil, err
	}
	ids := map[int64]bool{}
	var id int64
	_, err = pgx.ForEachRow(rows, []any{&id}, func() error {
		ids[id] = false
		return nil
	})
	return ids, err
}

var traceTables = []string{"trace_segments", "trace_segments_low", "trace_tags", "traces"}

// deleteTraces deletes the traces in groups. The geometry is read before
// the delete, to know which tiles to expire.
func deleteTraces(ctx context.Context, conn *pgx.Conn, ids []int64, exp *Expire) (int, error) {
	slices.Sort(ids)
	const group = 1000
	for i := 0; i < len(ids); i += group {
		part := ids[i:min(i+group, len(ids))]
		tx, err := conn.Begin(ctx)
		if err != nil {
			return i, err
		}
		if exp != nil {
			rows, err := tx.Query(ctx, "SELECT ST_AsBinary(geom) FROM trace_segments WHERE gpx_id = ANY($1)", part)
			if err != nil {
				tx.Rollback(ctx)
				return i, err
			}
			var wkb []byte
			_, err = pgx.ForEachRow(rows, []any{&wkb}, func() error {
				pts, err := ParseWKBLineString(wkb)
				exp.AddLine(pts)
				return err
			})
			if err == nil {
				err = exp.Flush()
			}
			if err != nil {
				tx.Rollback(ctx)
				return i, err
			}
		}
		for _, t := range traceTables {
			if _, err := tx.Exec(ctx, "DELETE FROM "+t+" WHERE gpx_id = ANY($1)", part); err != nil {
				tx.Rollback(ctx)
				return i, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return i, err
		}
	}
	return len(ids), nil
}

// batch keeps the rows of many traces as COPY text, until flush.
type batch struct {
	count                       int
	traces, tags, segments, low bytes.Buffer
	updTraces, updTags          bytes.Buffer
}

func (b *batch) addNew(t *Trace, cfg SplitConfig, exp *Expire) int {
	b.count++
	writeTrace(&b.traces, t)
	writeTags(&b.tags, t)
	n := 0
	for trackid, pts := range t.Segments {
		lines, low := BuildTrack(pts, cfg)
		for seg, l := range lines {
			fmt.Fprintf(&b.segments, "%d\t%d\t%d\t%s\t%s\t%s\n", t.ID, trackid, seg,
				hex.EncodeToString(EWKBLineString(l.Points)), copyTime(l.Start), copyTime(l.End))
			exp.AddLine(l.Points)
			n++
		}
		if len(low) > 0 {
			fmt.Fprintf(&b.low, "%d\t%d\t%s\n", t.ID, trackid, hex.EncodeToString(EWKBMultiLineString(low)))
		}
	}
	return n
}

func (b *batch) addUpdate(t *Trace) {
	b.count++
	writeTrace(&b.updTraces, t)
	writeTags(&b.updTags, t)
}

func writeTrace(w *bytes.Buffer, t *Trace) {
	w.WriteString(strconv.FormatInt(t.ID, 10))
	w.WriteByte('\t')
	w.WriteString(copyText(t.Name))
	w.WriteByte('\t')
	w.WriteString(copyText(t.Description))
	w.WriteByte('\n')
}

// writeTags writes each tag once. The primary key is (gpx_id, tag).
func writeTags(w *bytes.Buffer, t *Trace) {
	seen := map[string]bool{}
	for _, tag := range t.Tags {
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		fmt.Fprintf(w, "%d\t%s\n", t.ID, copyText(&tag))
	}
}

func (b *batch) flush(ctx context.Context, conn *pgx.Conn, exp *Expire) error {
	if b.count == 0 {
		return nil
	}
	// Tiles go to disk before the commit, so they are never lost.
	if err := exp.Flush(); err != nil {
		return err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	pg := tx.Conn().PgConn()
	copyIn := func(table string, buf *bytes.Buffer) error {
		if buf.Len() == 0 {
			return nil
		}
		_, err := pg.CopyFrom(ctx, buf, "COPY "+table+" FROM STDIN")
		if err != nil {
			return fmt.Errorf("copy %s: %w", table, err)
		}
		return nil
	}
	if err := copyIn("traces (gpx_id, name, description)", &b.traces); err != nil {
		return err
	}
	if err := copyIn("trace_tags (gpx_id, tag)", &b.tags); err != nil {
		return err
	}
	if err := copyIn("trace_segments (gpx_id, trackid, segment, geom, started_at, ended_at)", &b.segments); err != nil {
		return err
	}
	if err := copyIn("trace_segments_low (gpx_id, trackid, geom)", &b.low); err != nil {
		return err
	}

	if b.updTraces.Len() > 0 {
		err := execMulti(ctx, pg, `
			CREATE TEMP TABLE upd_traces (gpx_id bigint, name text, description text) ON COMMIT DROP;
			CREATE TEMP TABLE upd_tags (gpx_id bigint, tag text) ON COMMIT DROP`)
		if err != nil {
			return err
		}
		if err := copyIn("upd_traces", &b.updTraces); err != nil {
			return err
		}
		if err := copyIn("upd_tags", &b.updTags); err != nil {
			return err
		}
		// Only write rows that changed.
		err = execMulti(ctx, pg, `
			UPDATE traces t SET name = u.name, description = u.description
			  FROM upd_traces u
			 WHERE t.gpx_id = u.gpx_id
			   AND (t.name, t.description) IS DISTINCT FROM (u.name, u.description);
			DELETE FROM trace_tags t USING upd_traces u
			 WHERE t.gpx_id = u.gpx_id
			   AND NOT EXISTS (SELECT 1 FROM upd_tags n WHERE n.gpx_id = t.gpx_id AND n.tag = t.tag);
			INSERT INTO trace_tags (gpx_id, tag)
			SELECT gpx_id, tag FROM upd_tags
			ON CONFLICT DO NOTHING`)
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	*b = batch{}
	return nil
}

// execMulti runs SQL with many statements and no parameters.
func execMulti(ctx context.Context, pg *pgconn.PgConn, sql string) error {
	_, err := pg.Exec(ctx, sql).ReadAll()
	return err
}

var copyEscaper = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)

// copyText escapes a value for COPY text format. nil is NULL.
func copyText(s *string) string {
	if s == nil {
		return `\N`
	}
	return copyEscaper.Replace(*s)
}

func copyTime(t time.Time) string {
	if t.IsZero() {
		return `\N`
	}
	return t.UTC().Format(time.RFC3339Nano)
}
