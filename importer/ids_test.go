package importer

import (
	"context"
	"strings"
	"testing"
)

func TestReadIDList(t *testing.T) {
	ids, err := ReadIDList(strings.NewReader("# GPS trace IDs with identifiable visibility.\n20\n  24\n\n30\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != 20 || ids[1] != 24 || ids[2] != 30 {
		t.Errorf("ids = %v", ids)
	}
	for _, bad := range []string{"", "# only a comment\n", "20\nabc\n", "20\n-5\n"} {
		if _, err := ReadIDList(strings.NewReader(bad)); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestRunIDs(t *testing.T) {
	conn := testConn(t)
	ctx := context.Background()
	cfg := testConfig(t.TempDir())

	// Database: 100, 101, 102 from the dump, 110 from update -rss.
	if _, err := Run(ctx, conn, dump("2026-10-31T00:00:00Z", trk(100, "a")+trk(101, "")+trk(102, "")+trk(110, "")), cfg); err != nil {
		t.Fatal(err)
	}

	// List: 101 was deleted, 103 is new, 104 became identifiable but the
	// website no longer serves it. 110 is newer than the list.
	srv := fakeSite(t, nil, map[int64]string{103: gpx10}, nil)
	f := NewFetcher(srv.URL)
	f.Delay = 0
	st, err := RunIDs(ctx, conn, f, []int64{100, 102, 103, 104}, cfg, DefaultIDsConfig())
	if err != nil {
		t.Fatal(err)
	}
	if st.Deleted != 1 || st.Missing != 2 || st.Inserted != 1 || st.Skipped != 1 || st.Left != 0 {
		t.Fatalf("stats %+v", st)
	}
	if n := count(t, conn, "SELECT count(*) FROM traces WHERE gpx_id = 101"); n != 0 {
		t.Error("101 should be deleted")
	}
	if n := count(t, conn, "SELECT count(*) FROM trace_segments WHERE gpx_id = 101"); n != 0 {
		t.Error("101 segments should be deleted")
	}
	if n := count(t, conn, "SELECT count(*) FROM traces WHERE gpx_id IN (100, 102, 103, 110)"); n != 4 {
		t.Errorf("%d of 100, 102, 103, 110 left, want 4", n)
	}
	if st.ExpireFile == "" {
		t.Error("no expire file")
	}
}

// A new trace with no GPS time gets the download time: the current year.
func TestRunIDsYear(t *testing.T) {
	conn := testConn(t)
	ctx := context.Background()
	cfg := testConfig(t.TempDir())
	if _, err := Run(ctx, conn, dump("2026-10-31T00:00:00Z", trk(100, "")), cfg); err != nil {
		t.Fatal(err)
	}
	srv := fakeSite(t, nil, map[int64]string{103: gpx10}, nil)
	f := NewFetcher(srv.URL)
	f.Delay = 0
	if _, err := RunIDs(ctx, conn, f, []int64{100, 103}, cfg, DefaultIDsConfig()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, conn, "SELECT count(*) FROM trace_segments WHERE gpx_id = 103 AND year = extract(year FROM now())"); n != 1 {
		t.Errorf("trace 103 should have the current year")
	}
}

// A list that would delete most traces stops before any change.
func TestRunIDsMaxDelete(t *testing.T) {
	conn := testConn(t)
	ctx := context.Background()
	cfg := testConfig(t.TempDir())
	if _, err := Run(ctx, conn, dump("2026-10-31T00:00:00Z", trk(1, "")+trk(2, "")+trk(3, "")), cfg); err != nil {
		t.Fatal(err)
	}
	f := NewFetcher(fakeSite(t, nil, nil, nil).URL)
	f.Delay = 0
	if _, err := RunIDs(ctx, conn, f, []int64{3}, cfg, DefaultIDsConfig()); err == nil {
		t.Fatal("want an error")
	}
	if n := count(t, conn, "SELECT count(*) FROM traces"); n != 3 {
		t.Errorf("%d traces, want 3", n)
	}
}

// Missing traces over MaxTraces wait for the next run. A server error
// stops the run without losing the traces already loaded.
func TestRunIDsLimitAndError(t *testing.T) {
	conn := testConn(t)
	ctx := context.Background()
	cfg := testConfig(t.TempDir())
	if _, err := Run(ctx, conn, dump("2026-10-31T00:00:00Z", trk(1, "")), cfg); err != nil {
		t.Fatal(err)
	}
	fail := map[int64]bool{4: true}
	srv := fakeSite(t, nil, map[int64]string{2: gpx10, 3: gpx10, 4: gpx10, 5: gpx10}, fail)
	f := NewFetcher(srv.URL)
	f.Delay = 0
	list := []int64{1, 2, 3, 4, 5}

	st, err := RunIDs(ctx, conn, f, list, cfg, IDsConfig{MaxTraces: 1})
	if err != nil || st.Inserted != 1 || st.Left != 3 {
		t.Fatalf("limit run: %+v, %v", st, err)
	}
	st, err = RunIDs(ctx, conn, f, list, cfg, DefaultIDsConfig())
	if err == nil || st.Inserted != 1 || st.Left != 2 {
		t.Fatalf("error run: %+v, %v", st, err)
	}
	delete(fail, 4)
	st, err = RunIDs(ctx, conn, f, list, cfg, DefaultIDsConfig())
	if err != nil || st.Inserted != 2 || st.Left != 0 {
		t.Fatalf("last run: %+v, %v", st, err)
	}
	if n := count(t, conn, "SELECT count(*) FROM traces"); n != 5 {
		t.Errorf("%d traces, want 5", n)
	}
}
