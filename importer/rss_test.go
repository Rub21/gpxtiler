package importer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const gpx11 = `<?xml version="1.0" encoding="UTF-8"?>
<gpx version="1.1" creator="test" xmlns="http://www.topografix.com/GPX/1/1">
 <wpt lat="1" lon="1"><name>skip me</name></wpt>
 <trk><name>a</name>
  <trkseg>
   <trkpt lat="47.14" lon="9.52"><ele>450</ele><time>2013-08-01T10:00:00Z</time></trkpt>
   <trkpt lat="47.15" lon="9.53"><time>2013-08-01T10:01:00Z</time></trkpt>
  </trkseg>
  <trkseg></trkseg>
 </trk>
</gpx>
`

const gpx10 = `<?xml version="1.0"?>
<gpx version="1.0" xmlns="http://www.topografix.com/GPX/1/0">
 <trk><trkseg>
  <trkpt lat="10" lon="20"/><trkpt lat="10.1" lon="20.1"/>
 </trkseg></trk>
</gpx>
`

func TestParseOriginalLatin1(t *testing.T) {
	// A GPX in ISO-8859-1, with a name that has a byte that is not UTF-8.
	gpx := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\n" +
		"<gpx version=\"1.1\"><trk><name>Caf\xe9</name><trkseg>" +
		"<trkpt lat=\"1\" lon=\"2\"></trkpt><trkpt lat=\"1\" lon=\"2.1\"></trkpt>" +
		"</trkseg></trk></gpx>"
	segs, err := ParseOriginal(strings.NewReader(gpx))
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 || len(segs[0]) != 2 {
		t.Fatalf("got %d segments %v", len(segs), segs)
	}
}

func TestParseOriginal(t *testing.T) {
	segs, err := ParseOriginal(strings.NewReader(gpx11))
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 || len(segs[0]) != 2 || len(segs[1]) != 0 {
		t.Fatalf("got %d segments: %v", len(segs), segs)
	}
	if segs[0][0].Lat != 47.14 || segs[0][0].Time.IsZero() {
		t.Errorf("bad first point %+v", segs[0][0])
	}
}

// A tar upload comes as two GPX documents, one after the other.
func TestParseOriginalTar(t *testing.T) {
	segs, err := ParseOriginal(strings.NewReader(gpx11 + gpx10))
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 || len(segs[2]) != 2 {
		t.Fatalf("got %d segments: %v", len(segs), segs)
	}
}

func rssFeed(ids ...int64) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>t</title>`)
	for _, id := range ids {
		fmt.Fprintf(&b, `<item><title>x</title><link>https://www.openstreetmap.org/user/A%%20B/traces/%d</link><pubDate>Thu, 12 Nov 2026 21:08:54 +0000</pubDate></item>`, id)
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

func TestReadRSS(t *testing.T) {
	max, uploaded, err := ReadRSS(strings.NewReader(rssFeed(105, 110, 99)))
	if err != nil {
		t.Fatal(err)
	}
	if max != 110 {
		t.Errorf("max = %d, want 110", max)
	}
	// rssFeed gives every item the same pubDate.
	want := time.Date(2026, 11, 12, 21, 8, 54, 0, time.UTC)
	if len(uploaded) != 3 || !uploaded[105].Equal(want) {
		t.Errorf("uploaded = %v", uploaded)
	}
	if _, _, err := ReadRSS(strings.NewReader(rssFeed())); err == nil {
		t.Error("empty feed should fail")
	}
}

// fakeSite serves the RSS feed and the data of the public traces.
// Other ids answer 404, like trackable or deleted traces.
func fakeSite(t *testing.T, rssIDs []int64, public map[int64]string, fail map[int64]bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/traces/rss" {
			fmt.Fprint(w, rssFeed(rssIDs...))
			return
		}
		var id int64
		if _, err := fmt.Sscanf(r.URL.Path, "/traces/%d/data.gpx", &id); err != nil {
			http.NotFound(w, r)
			return
		}
		if fail[id] {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		body, ok := public[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// trk returns one dump trace with two points.
func trk(id int64, name string) string {
	n := ""
	if name != "" {
		n = "<name>" + name + "</name>"
	}
	return fmt.Sprintf(` <trk>%s<osm:id>%d</osm:id><trkseg><trkpt lat="47.1" lon="9.5"/><trkpt lat="47.2" lon="9.6"/></trkseg></trk>
`, n, id)
}

func TestRunRSS(t *testing.T) {
	conn := testConn(t)
	ctx := context.Background()
	dir := t.TempDir()
	cfg := testConfig(dir)

	// The dump loaded traces up to 100.
	if _, err := Run(ctx, conn, dump("2026-10-31T00:00:00Z", trk(100, "")), cfg); err != nil {
		t.Fatal(err)
	}

	// 101 trackable (404), 102 public, 103 has no points, 104 public.
	srv := fakeSite(t, []int64{104, 102}, map[int64]string{
		102: gpx11,
		103: `<gpx><wpt lat="1" lon="1"/></gpx>`,
		104: gpx10,
	}, nil)
	f := NewFetcher(srv.URL)
	f.Delay = 0
	rc := DefaultRSSConfig()

	st, err := RunRSS(ctx, conn, f, cfg, rc)
	if err != nil {
		t.Fatal(err)
	}
	if st.Checked != 4 || st.Inserted != 2 || st.Skipped != 2 || st.ToID != 104 {
		t.Fatalf("stats %+v", st)
	}
	if n := count(t, conn, "SELECT count(*) FROM traces WHERE gpx_id IN (102, 104)"); n != 2 {
		t.Errorf("%d new traces, want 2", n)
	}
	if n := count(t, conn, "SELECT count(*) FROM trace_segments WHERE gpx_id = 102"); n != 1 {
		t.Errorf("trace 102 has %d segments, want 1", n)
	}
	// 102 and 104 are in the feed: pubDate 2026-11-12.
	// gpx11 (102) has GPS times in 2013, before the upload: year 2013.
	if n := count(t, conn, "SELECT count(*) FROM trace_segments WHERE gpx_id = 102 AND year = 2013"); n != 1 {
		t.Errorf("trace 102 should have year 2013")
	}
	// gpx10 (104) has no times: the upload year from the feed.
	if n := count(t, conn, "SELECT count(*) FROM trace_segments WHERE gpx_id = 104 AND year = 2026"); n != 1 {
		t.Errorf("trace 104 should have year 2026")
	}
	if n := count(t, conn, "SELECT last_id FROM rss_state"); n != 104 {
		t.Errorf("last_id = %d, want 104", n)
	}
	if st.ExpireFile == "" {
		t.Error("no expire file")
	}

	// Nothing new: no requests for data, no changes.
	st, err = RunRSS(ctx, conn, f, cfg, rc)
	if err != nil || st.Checked != 0 {
		t.Fatalf("second run: %+v, %v", st, err)
	}
}

// A trace that is not in the feed gets the download time: the current year.
func TestRunRSSNotInFeed(t *testing.T) {
	conn := testConn(t)
	ctx := context.Background()
	cfg := testConfig(t.TempDir())
	if _, err := Run(ctx, conn, dump("2026-10-31T00:00:00Z", trk(100, "")), cfg); err != nil {
		t.Fatal(err)
	}
	// The feed only lists 104: 101 is behind.
	srv := fakeSite(t, []int64{104}, map[int64]string{101: gpx10, 104: gpx10}, nil)
	f := NewFetcher(srv.URL)
	f.Delay = 0
	if _, err := RunRSS(ctx, conn, f, cfg, DefaultRSSConfig()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, conn, "SELECT count(*) FROM trace_segments WHERE gpx_id = 101 AND year = extract(year FROM now())"); n != 1 {
		t.Errorf("trace 101 should have the current year")
	}
	if n := count(t, conn, "SELECT count(*) FROM trace_segments WHERE gpx_id = 104 AND year = 2026"); n != 1 {
		t.Errorf("trace 104 should have year 2026 from the feed")
	}
}

// A server error stops the run, and the next run starts again at that id.
func TestRunRSSServerError(t *testing.T) {
	conn := testConn(t)
	ctx := context.Background()
	cfg := testConfig(t.TempDir())
	if _, err := Run(ctx, conn, dump("2026-10-31T00:00:00Z", trk(100, "")), cfg); err != nil {
		t.Fatal(err)
	}
	fail := map[int64]bool{102: true}
	srv := fakeSite(t, []int64{103}, map[int64]string{101: gpx10, 102: gpx10, 103: gpx10}, fail)
	f := NewFetcher(srv.URL)
	f.Delay = 0

	st, err := RunRSS(ctx, conn, f, cfg, DefaultRSSConfig())
	if err == nil {
		t.Fatal("want an error")
	}
	if st.Inserted != 1 || st.ToID != 101 {
		t.Fatalf("stats %+v", st)
	}
	if n := count(t, conn, "SELECT last_id FROM rss_state"); n != 101 {
		t.Errorf("last_id = %d, want 101", n)
	}

	delete(fail, 102)
	st, err = RunRSS(ctx, conn, f, cfg, DefaultRSSConfig())
	if err != nil || st.Inserted != 2 || st.ToID != 103 {
		t.Fatalf("second run: %+v, %v", st, err)
	}
}

// Ids that a newer dump already loaded are not downloaded again.
func TestRunRSSSkipsLoaded(t *testing.T) {
	conn := testConn(t)
	ctx := context.Background()
	cfg := testConfig(t.TempDir())
	if _, err := Run(ctx, conn, dump("2026-10-31T00:00:00Z", trk(100, "")+trk(102, "")), cfg); err != nil {
		t.Fatal(err)
	}
	srv := fakeSite(t, []int64{102}, map[int64]string{101: gpx10}, map[int64]bool{102: true})
	f := NewFetcher(srv.URL)
	f.Delay = 0
	rc := DefaultRSSConfig()
	rc.StartID = 100
	st, err := RunRSS(ctx, conn, f, cfg, rc)
	if err != nil || st.Inserted != 1 || st.Skipped != 1 {
		t.Fatalf("%+v, %v", st, err)
	}
}

func TestRunRSSStartLatest(t *testing.T) {
	conn := testConn(t)
	ctx := context.Background()
	cfg := testConfig(t.TempDir())
	// The database has an old dump; the RSS is far ahead. With StartID -1
	// the traces in between are skipped and the next upload is loaded.
	if _, err := Run(ctx, conn, dump("2026-10-31T00:00:00Z", trk(100, "")), cfg); err != nil {
		t.Fatal(err)
	}
	srv := fakeSite(t, []int64{5000}, map[int64]string{5001: gpx10}, nil)
	f := NewFetcher(srv.URL)
	f.Delay = 0
	rc := DefaultRSSConfig()
	rc.StartID = -1
	st, err := RunRSS(ctx, conn, f, cfg, rc)
	if err != nil || st.Checked != 0 {
		t.Fatalf("first run: %+v, %v", st, err)
	}
	srv2 := fakeSite(t, []int64{5001}, map[int64]string{5001: gpx10}, nil)
	f2 := NewFetcher(srv2.URL)
	f2.Delay = 0
	st, err = RunRSS(ctx, conn, f2, cfg, rc)
	if err != nil || st.Inserted != 1 || st.FromID != 5001 {
		t.Fatalf("second run: %+v, %v", st, err)
	}
}

func TestRunRSSEmptyDatabase(t *testing.T) {
	conn := testConn(t)
	srv := fakeSite(t, []int64{5}, nil, nil)
	f := NewFetcher(srv.URL)
	f.Delay = 0
	if _, err := RunRSS(context.Background(), conn, f, testConfig(t.TempDir()), DefaultRSSConfig()); err == nil {
		t.Fatal("want an error on an empty database")
	}
}

// update classifies with the speeds stored by import: with 1,2,3 km/h a
// trace at about 20 km/h is a plane.
func TestRunRSSUsesStoredSpeeds(t *testing.T) {
	conn := testConn(t)
	ctx := context.Background()
	cfg := testConfig(t.TempDir())
	if _, err := Run(ctx, conn, dump("2026-10-31T00:00:00Z", trk(100, "")), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "UPDATE dump_state SET mode_speeds = '1,2,3'"); err != nil {
		t.Fatal(err)
	}
	ms, err := LoadModeSpeeds(ctx, conn)
	if err != nil || ms.String() != "1,2,3" {
		t.Fatalf("LoadModeSpeeds = %v, %v", ms, err)
	}
	// Five points, 10 s apart, about 20 km/h (about 55 m each).
	body := `<gpx version="1.1" xmlns="http://www.topografix.com/GPX/1/1"><trk><trkseg>`
	for i := 0; i < 5; i++ {
		body += fmt.Sprintf(`<trkpt lat="47.1" lon="%.6f"><time>2013-08-01T10:00:%02dZ</time></trkpt>`, 9.5+float64(i)*0.00073, i*10)
	}
	body += `</trkseg></trk></gpx>`
	srv := fakeSite(t, []int64{101}, map[int64]string{101: body}, nil)
	f := NewFetcher(srv.URL)
	f.Delay = 0
	if _, err := RunRSS(ctx, conn, f, cfg, DefaultRSSConfig()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, conn, "SELECT count(*) FROM trace_segments WHERE gpx_id = 101 AND mode = 4"); n != 1 {
		t.Errorf("trace 101 should be a plane with the stored speeds")
	}
}
