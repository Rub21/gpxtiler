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
	BatchSize      int        // traces per transaction
	MaxPoints      int        // cut lines after this many points
	MaxLength      float64    // or after this many meters
	LowZooms       []int      // simplified copies trace_segments_z<N>, one per zoom, for the low zoom tiles
	LowMaxJump     float64    // the simplified copies are cut at edges longer than this many meters
	Speeds         ModeSpeeds // upper speeds of walk, bike and vehicle; update reads them from dump_state
	ExpireDir      string     // "" disables expire files
	ExpireMinZoom  int        // expire files have the tiles from ExpireMinZoom to ExpireMaxZoom
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
		LowZooms:       []int{5, 8, 11},
		LowMaxJump:     2000,
		Speeds:         DefaultModeSpeeds(),
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
	var savedSpeeds *string
	err = conn.QueryRow(ctx, "SELECT timestamp, mode_speeds FROM dump_state").Scan(&last, &savedSpeeds)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		st.FirstLoad = true
	case err != nil:
		return st, err
	case rd.DumpTime.Before(last):
		return st, fmt.Errorf("dump time %s is older than the loaded dump %s",
			rd.DumpTime.Format(time.RFC3339), last.Format(time.RFC3339))
	case savedSpeeds != nil && *savedSpeeds != cfg.Speeds.String():
		return st, fmt.Errorf("the database was imported with -mode-speeds %s, not %s; drop the tables and import again",
			*savedSpeeds, cfg.Speeds.String())
	}

	if err := EnsureLowTables(ctx, conn, cfg.LowZooms); err != nil {
		return st, err
	}
	if err := ensureYearsToNow(ctx, conn, cfg.LowZooms); err != nil {
		return st, err
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
		parseErr = parseAll(pctx, ds, cfg.Workers, skipGeom, cfg.split(), rd.DumpTime, parsedCh)
		close(parseDone)
	}()
	fail := func(err error) (Stats, error) {
		cancel()
		for range parsedCh { // let the workers stop
		}
		<-parseDone
		return st, err
	}

	b := &batch{}
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
	if st.Deleted, err = deleteTraces(ctx, conn, gone, exp); err != nil {
		return st, err
	}

	_, err = conn.Exec(ctx, `INSERT INTO dump_state (id, timestamp, mode_speeds) VALUES (true, $1, $2)
		ON CONFLICT (id) DO UPDATE SET timestamp = EXCLUDED.timestamp, mode_speeds = EXCLUDED.mode_speeds`,
		rd.DumpTime, cfg.Speeds.String())
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
	return SplitConfig{MaxPoints: c.MaxPoints, MaxLength: c.MaxLength, LowZooms: c.LowZooms, LowMaxJump: c.LowMaxJump, Speeds: c.Speeds}
}

// LowTable is the name of the simplified copy for a low zoom.
func LowTable(z int) string {
	return fmt.Sprintf("trace_segments_z%d", z)
}

// LowZooms returns the zooms of the simplified copies in the database.
func LowZooms(ctx context.Context, conn *pgx.Conn) ([]int, error) {
	rows, err := conn.Query(ctx, `SELECT substr(table_name, 17)::int FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name ~ '^trace_segments_z[0-9]+$' ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int])
}

// LoadModeSpeeds returns the speeds of the import, from dump_state. No row
// or no value: the defaults.
func LoadModeSpeeds(ctx context.Context, conn *pgx.Conn) (ModeSpeeds, error) {
	var s *string
	err := conn.QueryRow(ctx, "SELECT mode_speeds FROM dump_state").Scan(&s)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && s == nil) {
		return DefaultModeSpeeds(), nil
	}
	if err != nil {
		return ModeSpeeds{}, err
	}
	return ParseModeSpeeds(*s)
}

// MinYear is the first partition. GPS times before it are not trusted
// (see validTime), so no row gets an older year.
const MinYear = 2000

// YearPartition is the name of the partition of table for one year.
func YearPartition(table string, year int) string {
	return fmt.Sprintf("%s_y%d", table, year)
}

// EnsureYears creates the partitions of trace_segments and of the
// simplified copies for zooms, from year from to year to. A row with a
// year without partition fails to insert, so import and update call this
// up to the next year before writing.
func EnsureYears(ctx context.Context, conn *pgx.Conn, zooms []int, from, to int) error {
	tables := []string{"trace_segments"}
	for _, z := range zooms {
		tables = append(tables, LowTable(z))
	}
	var sql strings.Builder
	for y := from; y <= to; y++ {
		for _, t := range tables {
			fmt.Fprintf(&sql, "CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM (%d) TO (%d);\n",
				YearPartition(t, y), t, y, y+1)
		}
	}
	return execMulti(ctx, conn.PgConn(), sql.String())
}

// ensureYearsToNow creates the partitions from MinYear to next year.
func ensureYearsToNow(ctx context.Context, conn *pgx.Conn, zooms []int) error {
	return EnsureYears(ctx, conn, zooms, MinYear, time.Now().UTC().Year()+1)
}

// EnsureLowTables creates the simplified copies for zooms. When the
// database already has traces with other copies, it is an error: the
// copies are only filled when a trace is loaded, so a new import is needed.
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
				gpx_id  bigint,
				trackid int,
				year    smallint NOT NULL,
				mode    smallint NOT NULL,
				geom    geometry(MultiLineString, 3857),
				PRIMARY KEY (gpx_id, trackid, year, mode)
			) PARTITION BY RANGE (year);
			CREATE INDEX IF NOT EXISTS %[1]s_geom_idx ON %[1]s USING gist (geom)`, LowTable(z)))
		if err != nil {
			return err
		}
	}
	return nil
}

// traceTables returns the tables with rows per trace, for deleteTraces.
func traceTables(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	zooms, err := LowZooms(ctx, conn)
	if err != nil {
		return nil, err
	}
	t := []string{"trace_segments"}
	for _, z := range zooms {
		t = append(t, LowTable(z))
	}
	return append(t, "trace_tags", "traces"), nil
}

// deleteTraces deletes the traces in groups. The geometry is read before
// the delete, to know which tiles to expire.
func deleteTraces(ctx context.Context, conn *pgx.Conn, ids []int64, exp *Expire) (int, error) {
	tables, err := traceTables(ctx, conn)
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
		for _, t := range tables {
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
	count                  int
	traces, tags, segments bytes.Buffer
	low                    map[int]*bytes.Buffer // per low zoom
	updTraces, updTags     bytes.Buffer
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
	for z, rows := range p.lowRows {
		if b.low == nil {
			b.low = map[int]*bytes.Buffer{}
		}
		if b.low[z] == nil {
			b.low[z] = &bytes.Buffer{}
		}
		b.low[z].Write(rows)
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
	if err := copyIn("trace_segments (gpx_id, trackid, segment, year, mode, geom, started_at, ended_at)", &b.segments); err != nil {
		return err
	}
	for z, buf := range b.low {
		if err := copyIn(LowTable(z)+" (gpx_id, trackid, year, mode, geom)", buf); err != nil {
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
