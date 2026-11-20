package importer

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// A segment with 3 points has 2 edges: too few for its own mode. It takes
// the mode of the whole trace, here the long bike segment.
func TestBuildRowsShortSegmentTakesTraceMode(t *testing.T) {
	t0 := time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)
	long := pathAt(30, 5.56, time.Second, t0) // 20 km/h
	short := pathAt(3, 5.56, time.Second, t0)
	for _, pts := range [][]Point{long, short} {
		for i := range pts {
			pts[i].Lat += 1
		}
	}
	tr := &Trace{ID: 7, Uploaded: uploaded, Segments: [][]Point{long, short}}
	cfg := SplitConfig{MaxPoints: 1000, MaxLength: 1e9, Speeds: DefaultModeSpeeds()}
	p := buildRows(tr, cfg)
	rows := strings.Split(strings.TrimSpace(string(bytes.TrimSpace(p.segRows))), "\n")
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	for i, row := range rows {
		f := strings.Split(row, "\t")
		if f[4] != "2" { // ModeBike
			t.Errorf("segment %d mode = %s, want 2 (bike)", i, f[4])
		}
	}
}

// A trace with only short segments has no mode.
func TestBuildRowsNoModeWhenTraceHasNone(t *testing.T) {
	pts := pathAt(3, 5, time.Second, time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC))
	for i := range pts {
		pts[i].Lat += 1
	}
	p := buildRows(&Trace{ID: 8, Uploaded: uploaded, Segments: [][]Point{pts}}, SplitConfig{MaxPoints: 1000, Speeds: DefaultModeSpeeds()})
	if f := strings.Split(string(p.segRows), "\t"); f[4] != "0" {
		t.Errorf("mode = %s, want 0", f[4])
	}
}

// Timed points at 0,0 (GPS without a fix) are dropped everywhere, so they
// do not feed the trace mode either: a long segment of them, with a
// 3-point real segment, gives no mode instead of walk.
func TestBuildRowsIgnoresNullIslandInTraceMode(t *testing.T) {
	t0 := time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)
	fake := make([]Point, 30) // all at 0,0, one per second: speed 0
	for i := range fake {
		fake[i].Time = t0.Add(time.Duration(i) * time.Second)
	}
	real := pathAt(3, 5.56, time.Second, t0)
	for i := range real {
		real[i].Lat += 1
	}
	p := buildRows(&Trace{ID: 9, Uploaded: uploaded, Segments: [][]Point{fake, real}}, SplitConfig{MaxPoints: 1000, Speeds: DefaultModeSpeeds()})
	rows := strings.Split(strings.TrimSpace(string(p.segRows)), "\n")
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1: the 0,0 segment is dropped", len(rows))
	}
	if f := strings.Split(rows[0], "\t"); f[4] != "0" {
		t.Errorf("mode = %s, want 0 (unknown), not walk from the 0,0 points", f[4])
	}
}
