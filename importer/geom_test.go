package importer

import (
	"math"
	"testing"
	"time"
)

var testUploaded = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// lowParts flattens the groups of one zoom, for tests that only count parts.
func lowParts(groups map[LowKey][][]XY) [][]XY {
	var out [][]XY
	for _, parts := range groups {
		out = append(out, parts...)
	}
	return out
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestToMercator(t *testing.T) {
	p := ToMercator(180, 0)
	if !near(p[0], 20037508.34, 0.01) || !near(p[1], 0, 1e-6) {
		t.Errorf("180,0 = %v", p)
	}
	// Known value for 9.52, 47.14 (Vaduz).
	p = ToMercator(9.52, 47.14)
	if !near(p[0], 1059761.55, 0.01) || !near(p[1], 5964955.65, 0.01) {
		t.Errorf("vaduz = %v", p)
	}
	// Latitude is limited to the Web Mercator square.
	if p := ToMercator(0, 90); !near(p[1], mercMax, 1) {
		t.Errorf("90 = %v", p)
	}
}

func TestBuildTrackCutsByPoints(t *testing.T) {
	var pts []Point
	for i := 0; i < 25; i++ {
		pts = append(pts, Point{Lon: 9 + float64(i)*0.0001, Lat: 47})
	}
	lines, lows := BuildTrack(pts, testUploaded, SplitConfig{MaxPoints: 10, MaxLength: 1e9, LowZooms: []int{8}})
	low := lowParts(lows[8])
	// 25 points, 10 per line, border point repeated: 10 + 10 + 7.
	if len(lines) != 3 {
		t.Fatalf("got %d lines", len(lines))
	}
	if len(lines[0].Points) != 10 || len(lines[1].Points) != 10 || len(lines[2].Points) != 7 {
		t.Errorf("sizes = %d %d %d", len(lines[0].Points), len(lines[1].Points), len(lines[2].Points))
	}
	if lines[0].Points[9] != lines[1].Points[0] {
		t.Errorf("border point not repeated")
	}
	if len(low) != 1 {
		t.Errorf("low parts = %d", len(low))
	}
}

func TestBuildTrackCutsByLength(t *testing.T) {
	// About 11 km between points on the equator.
	pts := []Point{{Lon: 0}, {Lon: 0.1}, {Lon: 0.2}, {Lon: 0.3}}
	lines, _ := BuildTrack(pts, testUploaded, SplitConfig{MaxPoints: 1000, MaxLength: 20000})
	if len(lines) != 2 || len(lines[0].Points) != 3 || len(lines[1].Points) != 2 {
		t.Fatalf("lines = %+v", lines)
	}
}

func TestBuildTrackDateline(t *testing.T) {
	pts := []Point{{Lon: 179, Lat: 10}, {Lon: 179.9, Lat: 10}, {Lon: -179.9, Lat: 12}, {Lon: -179, Lat: 12}}
	lines, lows := BuildTrack(pts, testUploaded, SplitConfig{MaxPoints: 1000, MaxLength: 1e9, LowZooms: []int{8}})
	low := lowParts(lows[8])
	if len(lines) != 2 || len(low) != 2 {
		t.Fatalf("lines=%d low=%d", len(lines), len(low))
	}
	end := lines[0].Points[len(lines[0].Points)-1]
	start := lines[1].Points[0]
	if !near(end[0], mercMax, 0.01) || !near(start[0], -mercMax, 0.01) {
		t.Errorf("edge points = %v %v", end, start)
	}
	if end[1] != start[1] {
		t.Errorf("both sides should have the same y")
	}
	// The crossing is half way, so its latitude is 11.
	if want := ToMercator(0, 11)[1]; !near(end[1], want, 0.01) {
		t.Errorf("crossing y = %v, want %v", end[1], want)
	}
}

// A GPS jump cuts the low zoom line, but not the normal lines.
func TestBuildTrackCutsLowAtJumps(t *testing.T) {
	pts := []Point{
		{Lon: 9.00, Lat: 47}, {Lon: 9.01, Lat: 47}, {Lon: 9.02, Lat: 47},
		{Lon: 9.50, Lat: 47}, // about 36 km away
		{Lon: 9.51, Lat: 47}, {Lon: 9.52, Lat: 47},
	}
	lines, lows := BuildTrack(pts, testUploaded, SplitConfig{MaxPoints: 1000, MaxLength: 1e9, LowZooms: []int{5, 8}, LowMaxJump: 2000})
	low := lowParts(lows[8])
	if len(lowParts(lows[5])) != 2 {
		t.Errorf("low z5 = %d parts, want 2", len(lowParts(lows[5])))
	}
	if len(lines) != 1 || len(lines[0].Points) != 6 {
		t.Errorf("lines = %d, points = %d, want 1 line with 6 points", len(lines), len(lines[0].Points))
	}
	if len(low) != 2 || len(low[0]) < 2 || len(low[1]) < 2 {
		t.Errorf("low = %d parts, want 2 cut at the jump", len(low))
	}
	// With 0 the jump is kept.
	if _, lows := BuildTrack(pts, testUploaded, SplitConfig{MaxPoints: 1000, MaxLength: 1e9, LowZooms: []int{8}}); len(lowParts(lows[8])) != 1 {
		t.Errorf("low = %d parts, want 1 with LowMaxJump 0", len(lowParts(lows[8])))
	}
}

func TestBuildTrackDropsShortPieces(t *testing.T) {
	lines, low := BuildTrack([]Point{{Lon: 1, Lat: 1}}, testUploaded, SplitConfig{MaxPoints: 1000, LowZooms: []int{8}})
	if len(lines) != 0 || len(low[8]) != 0 {
		t.Errorf("single point should give nothing")
	}
	lines, _ = BuildTrack([]Point{{Lon: 1, Lat: 1}, {Lon: 200, Lat: 1}}, testUploaded, SplitConfig{MaxPoints: 1000})
	if len(lines) != 0 {
		t.Errorf("invalid point should be dropped")
	}
}

func TestSimplify(t *testing.T) {
	pts := []XY{{0, 0}, {1, 0.1}, {2, 0}, {3, 5}, {4, 0}}
	got := Simplify(pts, 1)
	if len(got) != 4 || got[2] != (XY{3, 5}) {
		t.Errorf("got %v", got)
	}
}

func TestWKBRoundTrip(t *testing.T) {
	pts := []XY{{1, 2}, {3.5, -4.25}}
	got, err := ParseWKBLineString(EWKBLineString(pts))
	if err != nil || len(got) != 2 || got[1] != pts[1] {
		t.Errorf("got %v %v", got, err)
	}
}

// The simplified parts are grouped by year and mode: a walk in 2019 and a
// drive in 2020 give two rows for zoom 8.
func TestBuildTrackGroupsLowByYearAndMode(t *testing.T) {
	up := time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC)
	walk := pathAt(10, 1.39, time.Second, time.Date(2019, 12, 31, 23, 0, 0, 0, time.UTC))
	drive := pathAt(10, 25, time.Second, time.Date(2020, 1, 1, 1, 0, 0, 0, time.UTC))
	for i := range drive {
		drive[i].Lon += 1 // 111 km east: a GPS jump, so the low copy is cut here
	}
	pts := append(walk, drive...)
	lines, lows := BuildTrack(pts, up, SplitConfig{MaxPoints: 11, MaxLength: 1e9, LowZooms: []int{8}, LowMaxJump: 2000})
	// 20 points, 11 per line, border point repeated: 11 + 10.
	// Line 0 is the walk plus the first drive point; line 1 is the drive.
	if len(lines) != 2 {
		t.Fatalf("got %d lines", len(lines))
	}
	if lines[0].Year != 2019 || lines[0].Mode != ModeWalk {
		t.Errorf("line 0 = %d %d, want 2019 walk", lines[0].Year, lines[0].Mode)
	}
	if lines[1].Year != 2020 || lines[1].Mode != ModeVehicle {
		t.Errorf("line 1 = %d %d, want 2020 vehicle", lines[1].Year, lines[1].Mode)
	}
	low := lows[8]
	if len(low) != 2 {
		t.Fatalf("low groups = %v", low)
	}
	if len(low[LowKey{2019, ModeWalk}]) != 1 || len(low[LowKey{2020, ModeVehicle}]) != 1 {
		t.Errorf("low groups = %v", low)
	}
}

// Without times, the year is the upload year and the mode is unknown.
func TestBuildTrackNoTimes(t *testing.T) {
	up := time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC)
	pts := []Point{{Lon: 0}, {Lon: 0.001}, {Lon: 0.002}}
	lines, lows := BuildTrack(pts, up, SplitConfig{MaxPoints: 1000, MaxLength: 1e9, LowZooms: []int{8}})
	if len(lines) != 1 || lines[0].Year != 2020 || lines[0].Mode != ModeUnknown {
		t.Fatalf("lines = %+v", lines)
	}
	if len(lows[8][LowKey{2020, ModeUnknown}]) != 1 {
		t.Errorf("low = %v", lows[8])
	}
}
