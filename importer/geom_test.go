package importer

import (
	"math"
	"testing"
)

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
	lines, low := BuildTrack(pts, SplitConfig{MaxPoints: 10, MaxLength: 1e9})
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
	lines, _ := BuildTrack(pts, SplitConfig{MaxPoints: 1000, MaxLength: 20000})
	if len(lines) != 2 || len(lines[0].Points) != 3 || len(lines[1].Points) != 2 {
		t.Fatalf("lines = %+v", lines)
	}
}

func TestBuildTrackDateline(t *testing.T) {
	pts := []Point{{Lon: 179, Lat: 10}, {Lon: 179.9, Lat: 10}, {Lon: -179.9, Lat: 12}, {Lon: -179, Lat: 12}}
	lines, low := BuildTrack(pts, SplitConfig{MaxPoints: 1000, MaxLength: 1e9})
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

func TestBuildTrackDropsShortPieces(t *testing.T) {
	lines, low := BuildTrack([]Point{{Lon: 1, Lat: 1}}, SplitConfig{MaxPoints: 1000})
	if len(lines) != 0 || len(low) != 0 {
		t.Errorf("single point should give nothing")
	}
	lines, _ = BuildTrack([]Point{{Lon: 1, Lat: 1}, {Lon: 200, Lat: 1}}, SplitConfig{MaxPoints: 1000})
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
