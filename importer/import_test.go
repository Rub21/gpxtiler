package importer

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Rub21/gpxtiler/schema"
)

// testConn connects to GPXTILER_TEST_DB and creates the tables in a clean
// schema. The test is skipped when the variable is not set.
func testConn(t *testing.T) *pgx.Conn {
	t.Helper()
	dsn := os.Getenv("GPXTILER_TEST_DB")
	if dsn == "" {
		t.Skip("GPXTILER_TEST_DB is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	_, err = conn.PgConn().Exec(ctx, `
		CREATE EXTENSION IF NOT EXISTS postgis;
		DROP SCHEMA IF EXISTS gpxtiler_test CASCADE;
		CREATE SCHEMA gpxtiler_test;
		SET search_path TO gpxtiler_test, public`).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Apply(ctx, conn); err != nil {
		t.Fatal(err)
	}
	return conn
}

func testConfig(dir string) Config {
	cfg := DefaultConfig()
	cfg.ExpireDir = dir
	cfg.Log = log.New(io.Discard, "", 0)
	return cfg
}

func count(t *testing.T, conn *pgx.Conn, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func tags(t *testing.T, conn *pgx.Conn, id int64) []string {
	t.Helper()
	rows, _ := conn.Query(context.Background(), "SELECT tag FROM trace_tags WHERE gpx_id = $1 ORDER BY tag", id)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

const gpxHead = `<?xml version="1.0" encoding="UTF-8"?>
<gpx version="1.0" xmlns="http://www.topografix.com/GPX/1/0" xmlns:osm="https://www.openstreetmap.org/gpx/0.1">
 <time>%TIME%</time>
`

func dump(time, body string) io.Reader {
	return strings.NewReader(strings.Replace(gpxHead, "%TIME%", time, 1) + body + "</gpx>\n")
}

// Second dump: trace 1 changes name and tags (and has other points, which
// must be ignored), trace 2 is gone, traces 9 and 10 are new. Trace 10
// crosses the date line.
const dump2 = `
 <trk><name>Evening ride</name><desc>Vaduz &lt;loop&gt; &amp; back</desc><osm:id>1</osm:id>
  <osm:tag>bike</osm:tag><osm:tag>new</osm:tag><osm:tag>new</osm:tag>
  <trkseg><trkpt lat="1" lon="1"/><trkpt lat="2" lon="2"/></trkseg></trk>
 <trk><name>bad?name</name><desc>no url</desc><osm:id>6</osm:id>
  <trkseg><trkpt lat="47.16" lon="9.54"/></trkseg></trk>
 <trk><name>Single point</name><desc></desc><osm:id>8</osm:id><osm:tag>a&lt;b</osm:tag>
  <trkseg><trkpt lat="47.17" lon="9.55"/></trkseg></trk>
 <trk><osm:id>9</osm:id>
  <trkseg><trkpt lat="47.20" lon="9.50"><time>2020-01-01T00:00:00Z</time></trkpt>
   <trkpt lat="47.21" lon="9.51"/><trkpt lat="47.22" lon="9.52"><time>2020-01-01T00:10:00Z</time></trkpt></trkseg></trk>
 <trk><osm:id>10</osm:id>
  <trkseg><trkpt lat="10" lon="179.995"/><trkpt lat="10" lon="-179.995"/></trkseg></trk>
`

func TestImportSync(t *testing.T) {
	conn := testConn(t)
	ctx := context.Background()
	dir := t.TempDir()
	cfg := testConfig(dir)

	// First load: the fixture from planet-dump-ng.
	f, err := os.Open("../testdata/trackable.gpx")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := Run(ctx, conn, f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !st.FirstLoad || st.Inserted != 4 || st.Segments != 1 || st.ExpireFile != "" {
		t.Errorf("first load stats = %+v", st)
	}
	if n := count(t, conn, "SELECT count(*) FROM traces"); n != 4 {
		t.Errorf("traces = %d", n)
	}
	if n := count(t, conn, "SELECT count(*) FROM trace_grid_z8"); n == 0 {
		t.Error("no grid rows")
	}
	var name, desc string
	conn.QueryRow(ctx, "SELECT name, description FROM traces WHERE gpx_id = 1").Scan(&name, &desc)
	if name != "Morning ride" || desc != "Vaduz <loop> & back" {
		t.Errorf("trace 1 = %q %q", name, desc)
	}
	if n := count(t, conn, "SELECT count(*) FROM traces WHERE gpx_id = 2 AND name IS NULL"); n != 1 {
		t.Errorf("trace 2 name should be NULL")
	}
	if n := count(t, conn, "SELECT count(*) FROM trace_segments WHERE gpx_id = 1 AND ST_NPoints(geom) = 4 AND ST_SRID(geom) = 3857 AND started_at IS NOT NULL"); n != 1 {
		t.Errorf("trace 1 segment is wrong")
	}

	// Same dump again: nothing new, nothing deleted.
	f.Seek(0, io.SeekStart)
	st, err = Run(ctx, conn, f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Inserted != 0 || st.Updated != 4 || st.Deleted != 0 || st.ExpireFile != "" {
		t.Errorf("second run stats = %+v", st)
	}

	// Newer dump with changes.
	st, err = Run(ctx, conn, dump("2013-08-08T12:00:00Z", dump2), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Inserted != 2 || st.Updated != 3 || st.Deleted != 1 {
		t.Errorf("sync stats = %+v", st)
	}
	conn.QueryRow(ctx, "SELECT name FROM traces WHERE gpx_id = 1").Scan(&name)
	if name != "Evening ride" {
		t.Errorf("trace 1 name = %q", name)
	}
	if got := tags(t, conn, 1); !slices.Equal(got, []string{"bike", "new"}) {
		t.Errorf("trace 1 tags = %v", got)
	}
	// Geometry of a known trace is not read again.
	if n := count(t, conn, "SELECT count(*) FROM trace_segments WHERE gpx_id = 1 AND ST_NPoints(geom) = 4"); n != 1 {
		t.Errorf("trace 1 geometry changed")
	}
	if n := count(t, conn, "SELECT count(*) FROM traces WHERE gpx_id = 2"); n != 0 {
		t.Errorf("trace 2 not deleted")
	}
	if n := count(t, conn, "SELECT count(*) FROM trace_segments WHERE gpx_id = 10"); n != 2 {
		t.Errorf("trace 10 should be cut at the date line, got %d lines", n)
	}
	// Trace 10 is 1.1 km long: at z11 (76 m cells) it has edges on both
	// sides of the date line, none across it.
	if n := count(t, conn, "SELECT count(*) FROM trace_grid_z11 WHERE ST_XMax(geom) < -20030000"); n == 0 {
		t.Error("trace 10 should have edges on the west side of the date line")
	}
	if n := count(t, conn, "SELECT count(*) FROM trace_grid_z11 WHERE ST_XMin(geom) > 20030000"); n == 0 {
		t.Error("trace 10 should have edges on the east side of the date line")
	}
	if n := count(t, conn, "SELECT count(*) FROM trace_grid_z11 WHERE ST_XMin(geom) < 0 AND ST_XMax(geom) > 0"); n != 0 {
		t.Errorf("%d edges cross the date line", n)
	}
	if st.ExpireFile == "" || st.ExpireTiles == 0 {
		t.Fatalf("no expire file: %+v", st)
	}
	b, _ := os.ReadFile(st.ExpireFile)
	if !strings.Contains(string(b), "14/8624/5749\n") { // a tile of trace 9
		t.Errorf("expire file does not have trace 9: %q", b)
	}
	if parts, _ := filepath.Glob(filepath.Join(dir, "*.part")); len(parts) != 0 {
		t.Errorf("part files left: %v", parts)
	}

	// An older dump is rejected.
	if _, err := Run(ctx, conn, dump("2013-08-02T00:00:00Z", ""), cfg); err == nil || !strings.Contains(err.Error(), "older") {
		t.Errorf("older dump: err = %v", err)
	}

	// An empty dump is an error.
	if _, err := Run(ctx, conn, dump("2013-08-09T00:00:00Z", ""), cfg); err == nil || !strings.Contains(err.Error(), "no traces") {
		t.Errorf("empty dump: err = %v", err)
	}

	// A dump that would delete most traces stops before deleting.
	only10 := `<trk><osm:id>10</osm:id><trkseg><trkpt lat="10" lon="179.995"/><trkpt lat="10" lon="-179.995"/></trkseg></trk>`
	if _, err := Run(ctx, conn, dump("2013-08-09T00:00:00Z", only10), cfg); err == nil || !strings.Contains(err.Error(), "would delete") {
		t.Errorf("mass delete: err = %v", err)
	}
	if n := count(t, conn, "SELECT count(*) FROM traces"); n != 5 {
		t.Errorf("traces after refused run = %d", n)
	}
}

// A new dump does not delete traces with a higher id than the dump: they
// came from update -rss after the dump was made.
func TestImportKeepsNewerTraces(t *testing.T) {
	conn := testConn(t)
	ctx := context.Background()
	cfg := testConfig(t.TempDir())
	body := ` <trk><osm:id>%d</osm:id><trkseg><trkpt lat="47.1" lon="9.5"/><trkpt lat="47.2" lon="9.6"/></trkseg></trk>
`
	first := ""
	for _, id := range []int{1, 2, 3, 50} { // 50 came from the RSS
		first += strings.Replace(body, "%d", strconv.Itoa(id), 1)
	}
	if _, err := Run(ctx, conn, dump("2026-10-01T00:00:00Z", first), cfg); err != nil {
		t.Fatal(err)
	}
	// New dump up to id 10: trace 2 was deleted, 10 is new.
	second := ""
	for _, id := range []int{1, 3, 10} {
		second += strings.Replace(body, "%d", strconv.Itoa(id), 1)
	}
	st, err := Run(ctx, conn, dump("2026-11-01T00:00:00Z", second), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Deleted != 1 || st.Inserted != 1 {
		t.Fatalf("stats %+v", st)
	}
	if n := count(t, conn, "SELECT count(*) FROM traces WHERE gpx_id IN (1, 3, 10, 50)"); n != 4 {
		t.Errorf("%d of 1, 3, 10, 50 left, want 4", n)
	}
}

// Two traces on the same road share the grid rows; a delete takes them back.
func TestGridCounts(t *testing.T) {
	conn := testConn(t)
	ctx := context.Background()
	cfg := testConfig(t.TempDir())
	road := `<trk><osm:id>%d</osm:id><trkseg><trkpt lat="47.10" lon="9.50"/><trkpt lat="47.10" lon="9.52"/><trkpt lat="47.10" lon="9.54"/></trkseg></trk>
`
	other := `<trk><osm:id>3</osm:id><trkseg><trkpt lat="47.30" lon="9.50"/><trkpt lat="47.30" lon="9.52"/></trkseg></trk>
`
	body := strings.Replace(road, "%d", "1", 1) + strings.Replace(road, "%d", "2", 1) + other
	if _, err := Run(ctx, conn, dump("2026-10-01T00:00:00Z", body), cfg); err != nil {
		t.Fatal(err)
	}
	if n := count(t, conn, "SELECT count(*) FROM trace_grid_z11 WHERE n = 2"); n == 0 {
		t.Error("the shared road should have rows with n = 2")
	}
	if n := count(t, conn, "SELECT count(*) FROM trace_grid_z11 WHERE n NOT IN (1, 2)"); n != 0 {
		t.Errorf("%d rows with a wrong n", n)
	}
	// Delete trace 2: the road stays with n = 1.
	body = strings.Replace(road, "%d", "1", 1) + other
	if _, err := Run(ctx, conn, dump("2026-10-02T00:00:00Z", body), cfg); err != nil {
		t.Fatal(err)
	}
	if n := count(t, conn, "SELECT count(*) FROM trace_grid_z11 WHERE n <> 1"); n != 0 {
		t.Errorf("%d rows with n <> 1 after one delete", n)
	}
	// Delete trace 1 too: only the other trace is left.
	if _, err := Run(ctx, conn, dump("2026-10-03T00:00:00Z", other), cfg); err != nil {
		t.Fatal(err)
	}
	if n := count(t, conn, "SELECT count(*) FROM trace_grid_z11 WHERE ST_YMin(geom) < 5970000"); n != 0 {
		t.Errorf("%d rows of the road left after both deletes", n)
	}
	if n := count(t, conn, "SELECT count(*) FROM trace_grid_z11"); n == 0 {
		t.Error("the other trace should still have rows")
	}
}
