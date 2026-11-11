package importer

import (
	"math"
	"slices"
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
	lines, grid := BuildTrack(pts, SplitConfig{MaxPoints: 10, MaxLength: 1e9, LowZooms: []int{8}})
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
	// 270 m going east at z8 (600 m cells): one edge or two, eastward.
	if len(grid[8]) < 1 {
		t.Errorf("grid edges = %v", grid[8])
	}
	for _, e := range grid[8] {
		if e.X1 != e.X0+1 || e.Y1 != e.Y0 {
			t.Errorf("edge %v is not one cell east", e)
		}
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
	lines, grid := BuildTrack(pts, SplitConfig{MaxPoints: 1000, MaxLength: 1e9, LowZooms: []int{8}})
	if len(lines) != 2 || len(grid[8]) < 2 {
		t.Fatalf("lines=%d grid=%d", len(lines), len(grid[8]))
	}
	// No edge crosses the date line: every edge stays on one side.
	limit := int32(math.Round(mercMax / LowTolerance(8)))
	for _, e := range grid[8] {
		if (e.X0 > 0) != (e.X1 > 0) || e.X0 > limit || e.X0 < -limit {
			t.Errorf("edge %v crosses the date line", e)
		}
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

// A GPS jump cuts the grid edges, but not the normal lines.
func TestBuildTrackCutsGridAtJumps(t *testing.T) {
	pts := []Point{
		{Lon: 9.00, Lat: 47}, {Lon: 9.01, Lat: 47}, {Lon: 9.02, Lat: 47},
		{Lon: 9.50, Lat: 47}, // about 36 km away
		{Lon: 9.51, Lat: 47}, {Lon: 9.52, Lat: 47},
	}
	lines, grid := BuildTrack(pts, SplitConfig{MaxPoints: 1000, MaxLength: 1e9, LowZooms: []int{5, 8}, LowMaxJump: 2000})
	if len(lines) != 1 || len(lines[0].Points) != 6 {
		t.Errorf("lines = %d, points = %d, want 1 line with 6 points", len(lines), len(lines[0].Points))
	}
	longest := func(edges []GridEdge) int32 {
		var m int32
		for _, e := range edges {
			m = max(m, e.X1-e.X0)
		}
		return m
	}
	// At z5 (about 4.9 km cells) the jump would be about 7 cells long.
	if m := longest(grid[5]); m > 1 {
		t.Errorf("grid z5 has an edge %d cells long: the jump was not cut", m)
	}
	// At z8 (600 m cells) the jump would be about 60 cells long.
	if m := longest(grid[8]); m > 3 {
		t.Errorf("grid z8 has an edge %d cells long: the jump was not cut", m)
	}
	// With 0 the jump is kept.
	if _, grid := BuildTrack(pts, SplitConfig{MaxPoints: 1000, MaxLength: 1e9, LowZooms: []int{8}}); longest(grid[8]) < 50 {
		t.Errorf("grid z8 = %v, want the jump with LowMaxJump 0", grid[8])
	}
}

func TestGridEdges(t *testing.T) {
	// Two lines on the same road, one going back: each edge once per direction.
	cell := LowTolerance(8)
	a := XY{10 * cell, 20 * cell}
	b := XY{11 * cell, 20 * cell}
	c := XY{12 * cell, 20 * cell}
	lines := []Line{{Points: []XY{a, b, c}}, {Points: []XY{c, b, a, b, c}}}
	got := GridEdges(lines, []int{8}, 0)[8]
	want := []GridEdge{{10, 20, 11, 20}, {11, 20, 12, 20}, {12, 20, 11, 20}, {11, 20, 10, 20}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// Points inside one cell give no edge.
	got = GridEdges([]Line{{Points: []XY{a, {a[0] + cell/4, a[1]}}}}, []int{8}, 0)[8]
	if len(got) != 0 {
		t.Errorf("same cell: got %v", got)
	}
	// A jump is cut: no edge across it.
	far := XY{a[0] + 50000, a[1]}
	got = GridEdges([]Line{{Points: []XY{a, b, far, {far[0] + cell, far[1]}}}}, []int{8}, 2000)[8]
	if len(got) != 2 {
		t.Errorf("jump: got %v, want 2 edges", got)
	}
	// A zoom with no lines has no edges, but is in the map.
	if edges, ok := GridEdges(nil, []int{5}, 0)[5]; !ok || len(edges) != 0 {
		t.Errorf("empty: %v %v", edges, ok)
	}
}

func TestBuildTrackDropsShortPieces(t *testing.T) {
	lines, grid := BuildTrack([]Point{{Lon: 1, Lat: 1}}, SplitConfig{MaxPoints: 1000, LowZooms: []int{8}})
	if len(lines) != 0 || len(grid[8]) != 0 {
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
