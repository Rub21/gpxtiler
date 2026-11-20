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
	lines, lows := BuildTrack(pts, testUploaded, ModeUnknown, SplitConfig{MaxPoints: 10, MaxLength: 1e9, LowZooms: []int{8}})
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
	pts := []Point{{Lon: 0, Lat: 0.0001}, {Lon: 0.1}, {Lon: 0.2}, {Lon: 0.3}}
	lines, _ := BuildTrack(pts, testUploaded, ModeUnknown, SplitConfig{MaxPoints: 1000, MaxLength: 20000})
	if len(lines) != 2 || len(lines[0].Points) != 3 || len(lines[1].Points) != 2 {
		t.Fatalf("lines = %+v", lines)
	}
}

func TestBuildTrackDateline(t *testing.T) {
	pts := []Point{{Lon: 179, Lat: 10}, {Lon: 179.9, Lat: 10}, {Lon: -179.9, Lat: 12}, {Lon: -179, Lat: 12}}
	lines, lows := BuildTrack(pts, testUploaded, ModeUnknown, SplitConfig{MaxPoints: 1000, MaxLength: 1e9, LowZooms: []int{8}})
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
	lines, lows := BuildTrack(pts, testUploaded, ModeUnknown, SplitConfig{MaxPoints: 1000, MaxLength: 1e9, LowZooms: []int{5, 8}, LowMaxJump: 2000})
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
	if _, lows := BuildTrack(pts, testUploaded, ModeUnknown, SplitConfig{MaxPoints: 1000, MaxLength: 1e9, LowZooms: []int{8}}); len(lowParts(lows[8])) != 1 {
		t.Errorf("low = %d parts, want 1 with LowMaxJump 0", len(lowParts(lows[8])))
	}
}

func TestBuildTrackDropsShortPieces(t *testing.T) {
	lines, low := BuildTrack([]Point{{Lon: 1, Lat: 1}}, testUploaded, ModeUnknown, SplitConfig{MaxPoints: 1000, LowZooms: []int{8}})
	if len(lines) != 0 || len(low[8]) != 0 {
		t.Errorf("single point should give nothing")
	}
	lines, _ = BuildTrack([]Point{{Lon: 1, Lat: 1}, {Lon: 200, Lat: 1}}, testUploaded, ModeUnknown, SplitConfig{MaxPoints: 1000})
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
// drive in 2020 are two <trkseg>, so two calls, and give two rows for zoom 8.
func TestBuildTrackGroupsLowByYearAndMode(t *testing.T) {
	up := time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC)
	cfg := SplitConfig{MaxPoints: 1000, MaxLength: 1e9, LowZooms: []int{8}, LowMaxJump: 2000, Speeds: DefaultModeSpeeds()}
	walk := pathAt(10, 1.39, time.Second, time.Date(2019, 12, 31, 23, 0, 0, 0, time.UTC))
	drive := pathAt(10, 25, time.Second, time.Date(2020, 1, 1, 1, 0, 0, 0, time.UTC))
	for i := range walk {
		walk[i].Lat += 1
	}
	for i := range drive {
		drive[i].Lat += 2
	}
	wl, wlow := BuildTrack(walk, up, ModeUnknown, cfg)
	dl, dlow := BuildTrack(drive, up, ModeUnknown, cfg)
	if len(wl) != 1 || wl[0].Year != 2019 || wl[0].Mode != ModeWalk {
		t.Errorf("walk lines = %+v, want one 2019 walk", wl)
	}
	if len(dl) != 1 || dl[0].Year != 2020 || dl[0].Mode != ModeVehicle {
		t.Errorf("drive lines = %+v, want one 2020 vehicle", dl)
	}
	if len(wlow[8][LowKey{2019, ModeWalk}]) != 1 || len(wlow[8]) != 1 {
		t.Errorf("walk low = %v", wlow[8])
	}
	if len(dlow[8][LowKey{2020, ModeVehicle}]) != 1 || len(dlow[8]) != 1 {
		t.Errorf("drive low = %v", dlow[8])
	}
}

// One <trkseg> has one mode: every chunk and every jump part gets it, the
// year stays per line.
func TestBuildTrackOneModePerPiece(t *testing.T) {
	up := time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC)
	pts := pathAt(20, 1.39, time.Second, time.Date(2019, 12, 31, 23, 59, 55, 0, time.UTC))
	for i := range pts {
		pts[i].Lat += 1
	}
	for i := 10; i < 20; i++ {
		pts[i].Lon += 1 // a jump of 111 km in one second, left out of the median
	}
	lines, lows := BuildTrack(pts, up, ModeUnknown, SplitConfig{MaxPoints: 11, MaxLength: 1e9, LowZooms: []int{8}, LowMaxJump: 2000, Speeds: DefaultModeSpeeds()})
	if len(lines) != 2 {
		t.Fatalf("got %d lines", len(lines))
	}
	for i, l := range lines {
		if l.Mode != ModeWalk {
			t.Errorf("line %d mode = %d, want walk", i, l.Mode)
		}
	}
	if lines[0].Year != 2019 || lines[1].Year != 2020 {
		t.Errorf("years = %d %d, want 2019 2020", lines[0].Year, lines[1].Year)
	}
	for key := range lows[8] {
		if key.Mode != ModeWalk {
			t.Errorf("low key %v: want walk", key)
		}
	}
	if len(lows[8]) != 2 {
		t.Errorf("low groups = %v, want 2019 and 2020", lows[8])
	}
}

// A piece with fewer than 3 timed edges takes the fallback mode.
func TestBuildTrackFallbackMode(t *testing.T) {
	pts := pathAt(3, 5, time.Second, time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC))
	for i := range pts {
		pts[i].Lat += 1
	}
	lines, lows := BuildTrack(pts, uploaded, ModeBike, SplitConfig{MaxPoints: 1000, MaxLength: 1e9, LowZooms: []int{8}, Speeds: DefaultModeSpeeds()})
	if len(lines) != 1 || lines[0].Mode != ModeBike {
		t.Fatalf("lines = %+v, want bike", lines)
	}
	if len(lows[8][LowKey{2020, ModeBike}]) != 1 {
		t.Errorf("low = %v", lows[8])
	}
}

// GPS without a fix writes 0,0: the point is dropped, the line does not jump.
func TestBuildTrackDropsNullIsland(t *testing.T) {
	pts := []Point{{Lon: 9.5, Lat: 47}, {Lon: 0, Lat: 0}, {Lon: 9.51, Lat: 47}}
	lines, lows := BuildTrack(pts, testUploaded, ModeUnknown, SplitConfig{MaxPoints: 1000, MaxLength: 1e9, LowZooms: []int{8}, LowMaxJump: 2000})
	if len(lines) != 1 || len(lines[0].Points) != 2 {
		t.Fatalf("lines = %+v, want one line of 2 points", lines)
	}
	if len(lowParts(lows[8])) != 1 {
		t.Errorf("low = %v, want one part (no jump)", lows[8])
	}
}

// Without times, the year is the upload year and the mode is unknown.
func TestBuildTrackNoTimes(t *testing.T) {
	up := time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC)
	pts := []Point{{Lon: 0, Lat: 0.0001}, {Lon: 0.001}, {Lon: 0.002}}
	lines, lows := BuildTrack(pts, up, ModeUnknown, SplitConfig{MaxPoints: 1000, MaxLength: 1e9, LowZooms: []int{8}, Speeds: DefaultModeSpeeds()})
	if len(lines) != 1 || lines[0].Year != 2020 || lines[0].Mode != ModeUnknown {
		t.Fatalf("lines = %+v", lines)
	}
	if len(lows[8][LowKey{2020, ModeUnknown}]) != 1 {
		t.Errorf("low = %v", lows[8])
	}
}
