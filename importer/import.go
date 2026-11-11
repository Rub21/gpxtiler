// Package importer loads the planet-dump-ng GPX dump into PostGIS.
//
// Each run syncs the database with the dump:
//   - new id: insert the trace and its geometry
//   - known id: update name, description and tags (geometry never changes)
//   - id not in the dump: the trace was deleted or is no longer
//     identifiable, delete it. Ids higher than the highest id in the dump
//     are kept, because they are newer than the dump.
package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"runtime"
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
	LowZooms       []int   // grid tables trace_grid_z<N>, one per zoom, for the low zoom tiles
	LowMaxJump     float64 // the grid edges are cut at GPS jumps longer than this many meters
	ExpireDir      string  // "" disables expire files
	ExpireMinZoom  int     // expire files have the tiles from ExpireMinZoom to ExpireMaxZoom
	ExpireMaxZoom  int
	MaxDeleteRatio float64 // stop if the run would delete more than this share of traces
	Workers        int     // goroutines parsing the dump
	Log            *log.Logger
}

// DefaultConfig returns the default options.
func DefaultConfig() Config {
	return Config{
		BatchSize:      1000,
		MaxPoints:      1000,
		MaxLength:      50000,
		LowZooms:       []int{5, 8, 11, 14},
		LowMaxJump:     2000,
		ExpireMinZoom:  0,
		ExpireMaxZoom:  14,
		MaxDeleteRatio: 0.5,
		Workers:        min(runtime.NumCPU(), 8),
		Log:            log.Default(),
	}
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
	rd, ds, err := openDump(in)
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

	if err := EnsureLowTables(ctx, conn, cfg.LowZooms); err != nil {
		return st, err
	}
	if err := checkGrid(ctx, conn, cfg.LowZooms); err != nil {
		return st, err
	}
	// id -> seen in this dump
	ids, err := loadIDs(ctx, conn)
	if err != nil {
		return st, err
	}
	existing := len(ids)
	cfg.Log.Printf("dump time %s, %d traces in the database", rd.DumpTime.Format(time.RFC3339), existing)

	// An empty database is a bulk load: indexes at the end (see bulk.go).
	bulk := st.FirstLoad && existing == 0
	if bulk {
		cfg.Log.Printf("empty database: bulk load, the indexes are created at the end")
		if err := bulkStart(ctx, conn, cfg.LowZooms); err != nil {
			return st, err
		}
	}

	// The first load writes no expire file, the first render is complete.
	var exp *Expire
	if !st.FirstLoad && cfg.ExpireDir != "" {
		if exp, err = NewExpire(cfg.ExpireDir, cfg.ExpireMinZoom, cfg.ExpireMaxZoom, time.Now()); err != nil {
			return st, err
		}
	}

	// The workers read their own copy of the loaded ids; this loop writes
	// the ones seen in the dump into ids.
	known := make(map[int64]struct{}, len(ids))
	for id := range ids {
		known[id] = struct{}{}
	}
	skipGeom := func(id int64) bool { _, ok := known[id]; return ok }
	parsedCh := make(chan *parsed, 2*max(1, cfg.Workers))
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var parseErr error
	parseDone := make(chan struct{})
	go func() {
		parseErr = parseAll(pctx, ds, cfg.Workers, skipGeom, cfg.split(), parsedCh)
		close(parseDone)
	}()
	fail := func(err error) (Stats, error) {
		cancel()
		for range parsedCh { // let the workers stop
		}
		<-parseDone
		return st, err
	}

	b := &batch{bulk: bulk}
	total := 0
	var dumpMax int64 // highest trace id in the dump
	for p := range parsedCh {
		t := p.t
		total++
		dumpMax = max(dumpMax, t.ID)
		seen, known := ids[t.ID]
		switch {
		case seen:
			cfg.Log.Printf("trace %d is twice in the dump, using the first one", t.ID)
			continue
		case known:
			b.addUpdate(t)
			st.Updated++
		default:
			st.Segments += b.addParsed(p, exp)
			st.Inserted++
		}
		ids[t.ID] = true
		if b.count >= cfg.BatchSize {
			if err := b.flush(ctx, conn, exp); err != nil {
				return fail(err)
			}
		}
		if total%100000 == 0 {
			cfg.Log.Printf("%d traces read (%d new, %d updated)", total, st.Inserted, st.Updated)
		}
	}
	<-parseDone
	if parseErr != nil {
		return st, parseErr
	}
	if err := b.flush(ctx, conn, exp); err != nil {
		return st, err
	}
	if bulk {
		if err := bulkFinish(ctx, conn, cfg.LowZooms, cfg.Log); err != nil {
			return st, err
		}
	}

	if total == 0 && existing > 0 {
		return st, errors.New("the dump has no traces; check the dump")
	}

	// Ids higher than the highest id in the dump are kept: they are newer
	// than the dump and came from "update -rss" or "update -ids".
	var gone []int64
	for id, seen := range ids {
		if !seen && id <= dumpMax {
			gone = append(gone, id)
		}
	}
	if existing > 0 && cfg.MaxDeleteRatio > 0 && float64(len(gone)) > cfg.MaxDeleteRatio*float64(existing) {
		return st, fmt.Errorf("the dump would delete %d of %d traces; check the dump or raise -max-delete-ratio",
			len(gone), existing)
	}
	if st.Deleted, err = deleteTraces(ctx, conn, gone, exp, cfg); err != nil {
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

func (c Config) split() SplitConfig {
	return SplitConfig{MaxPoints: c.MaxPoints, MaxLength: c.MaxLength, LowZooms: c.LowZooms, LowMaxJump: c.LowMaxJump}
}

// LowTable is the name of the grid table for a low zoom.
func LowTable(z int) string {
	return fmt.Sprintf("trace_grid_z%d", z)
}

// LowZooms returns the zooms of the grid tables in the database.
func LowZooms(ctx context.Context, conn *pgx.Conn) ([]int, error) {
	rows, err := conn.Query(ctx, `SELECT substr(table_name, 13)::int FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name ~ '^trace_grid_z[0-9]+$' ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int])
}

// EnsureLowTables creates the grid tables for zooms. When the database
// already has traces with other tables, it is an error: the tables are
// only filled when a trace is loaded, so a new import is needed.
func EnsureLowTables(ctx context.Context, conn *pgx.Conn, zooms []int) error {
	have, err := LowZooms(ctx, conn)
	if err != nil {
		return err
	}
	want := slices.Clone(zooms)
	slices.Sort(want)
	if len(have) > 0 && !slices.Equal(have, want) {
		var n int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM traces").Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("the database has the low zoom tables for zooms %v, not %v; drop the tables and import again", have, want)
		}
	}
	for _, z := range zooms {
		if z < 0 || z > 20 {
			return fmt.Errorf("low zoom %d is out of range", z)
		}
		err := execMulti(ctx, conn.PgConn(), fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS %[1]s (
				x0   int NOT NULL,
				y0   int NOT NULL,
				x1   int NOT NULL,
				y1   int NOT NULL,
				n    int NOT NULL,
				geom geometry(LineString, 3857) NOT NULL,
				PRIMARY KEY (x0, y0, x1, y1)
			);
			CREATE INDEX IF NOT EXISTS %[1]s_geom_idx ON %[1]s USING gist (geom)`, LowTable(z)))
		if err != nil {
			return err
		}
	}
	return nil
}

// traceTables are the tables with rows per trace, for deleteTraces.
var traceTables = []string{"trace_segments", "trace_tags", "traces"}

// deleteTraces deletes the traces in groups. The lines are read before
// the delete, to know which tiles to expire and which grid edges to take
// back (built again with the same cut as the insert: cfg.LowMaxJump).
func deleteTraces(ctx context.Context, conn *pgx.Conn, ids []int64, exp *Expire, cfg Config) (int, error) {
	zooms, err := LowZooms(ctx, conn)
	if err != nil {
		return 0, err
	}
	slices.Sort(ids)
	const group = 1000
	for i := 0; i < len(ids); i += group {
		part := ids[i:min(i+group, len(ids))]
		tx, err := conn.Begin(ctx)
		if err != nil {
			return i, err
		}
		rows, err := tx.Query(ctx, "SELECT gpx_id, ST_AsBinary(geom) FROM trace_segments WHERE gpx_id = ANY($1) ORDER BY gpx_id, trackid, segment", part)
		if err != nil {
			tx.Rollback(ctx)
			return i, err
		}
		lines := map[int64][]Line{}
		var id int64
		var wkb []byte
		_, err = pgx.ForEachRow(rows, []any{&id, &wkb}, func() error {
			pts, err := ParseWKBLineString(wkb)
			exp.AddLine(pts)
			lines[id] = append(lines[id], Line{Points: pts})
			return err
		})
		if err == nil {
			err = exp.Flush()
		}
		if err != nil {
			tx.Rollback(ctx)
			return i, err
		}
		gone := map[int]*bytes.Buffer{}
		for _, l := range lines {
			for z, edges := range GridEdges(l, zooms, cfg.LowMaxJump) {
				if gone[z] == nil {
					gone[z] = &bytes.Buffer{}
				}
				gone[z].Write(gridRows(edges))
			}
		}
		for z, buf := range gone {
			if err := gridRemove(ctx, tx.Conn().PgConn(), z, buf.Bytes()); err != nil {
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

// gridAdd adds edges (COPY text rows) to the grid table of zoom z: a new
// row gets n = 1, a known row n + 1. It runs inside a transaction.
func gridAdd(ctx context.Context, pg *pgconn.PgConn, z int, rows []byte) error {
	if len(rows) == 0 {
		return nil
	}
	table := LowTable(z)
	if err := execMulti(ctx, pg, "CREATE TEMP TABLE grid_in (x0 int, y0 int, x1 int, y1 int) ON COMMIT DROP"); err != nil {
		return err
	}
	if _, err := pg.CopyFrom(ctx, bytes.NewReader(rows), "COPY grid_in FROM STDIN"); err != nil {
		return fmt.Errorf("copy %s: %w", table, err)
	}
	return execMulti(ctx, pg, fmt.Sprintf(`
		INSERT INTO %[1]s (x0, y0, x1, y1, n, geom)
		SELECT x0, y0, x1, y1, count(*), %[2]s
		  FROM grid_in GROUP BY 1, 2, 3, 4
		ON CONFLICT (x0, y0, x1, y1) DO UPDATE SET n = %[1]s.n + EXCLUDED.n;
		DROP TABLE grid_in`, table, gridGeomSQL(z)))
}

// gridRemove takes edges (COPY text rows) back from the grid table of
// zoom z. A row that reaches 0 is deleted. It runs inside a transaction.
func gridRemove(ctx context.Context, pg *pgconn.PgConn, z int, rows []byte) error {
	if len(rows) == 0 {
		return nil
	}
	table := LowTable(z)
	if err := execMulti(ctx, pg, "CREATE TEMP TABLE grid_del (x0 int, y0 int, x1 int, y1 int) ON COMMIT DROP"); err != nil {
		return err
	}
	if _, err := pg.CopyFrom(ctx, bytes.NewReader(rows), "COPY grid_del FROM STDIN"); err != nil {
		return fmt.Errorf("copy %s: %w", table, err)
	}
	return execMulti(ctx, pg, fmt.Sprintf(`
		UPDATE %[1]s g SET n = g.n - d.c
		  FROM (SELECT x0, y0, x1, y1, count(*) AS c FROM grid_del GROUP BY 1, 2, 3, 4) d
		 WHERE g.x0 = d.x0 AND g.y0 = d.y0 AND g.x1 = d.x1 AND g.y1 = d.y1;
		DELETE FROM %[1]s WHERE n <= 0;
		DROP TABLE grid_del`, table))
}

// batch keeps the rows of many traces as COPY text, until flush.
type batch struct {
	count                  int
	traces, tags, segments bytes.Buffer
	grid                   map[int]*bytes.Buffer // per low zoom, trace_grid_z<N> rows
	updTraces, updTags     bytes.Buffer
	bulk                   bool // first load: the edges go to grid_raw_z<N> (see bulk.go)
}

func (b *batch) addNew(t *Trace, cfg SplitConfig, exp *Expire) int {
	return b.addParsed(buildRows(t, cfg), exp)
}

// addParsed adds a new trace with its rows already built.
func (b *batch) addParsed(p *parsed, exp *Expire) int {
	b.count++
	writeTrace(&b.traces, p.t)
	writeTags(&b.tags, p.t)
	b.segments.Write(p.segRows)
	for _, l := range p.lines {
		exp.AddLine(l.Points)
	}
	for z, rows := range p.gridRows {
		if b.grid == nil {
			b.grid = map[int]*bytes.Buffer{}
		}
		if b.grid[z] == nil {
			b.grid[z] = &bytes.Buffer{}
		}
		b.grid[z].Write(rows)
	}
	return p.n
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
	for z, buf := range b.grid {
		if b.bulk {
			if err := copyIn(rawTable(z)+" (x0, y0, x1, y1)", buf); err != nil {
				return err
			}
			continue
		}
		if err := gridAdd(ctx, pg, z, buf.Bytes()); err != nil {
			return err
		}
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
	*b = batch{bulk: b.bulk}
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
