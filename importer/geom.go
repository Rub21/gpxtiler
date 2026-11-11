package importer

import (
	"math"
	"time"
)

const (
	earthRadius = 6378137.0
	mercMax     = math.Pi * earthRadius // 20037508.34
	maxLat      = 85.0511287798066      // Web Mercator limit
)

// XY is a point in Web Mercator (EPSG:3857), in meters.
type XY [2]float64

// ToMercator converts lon/lat (EPSG:4326) to Web Mercator.
func ToMercator(lon, lat float64) XY {
	lat = math.Max(-maxLat, math.Min(maxLat, lat))
	x := lon * mercMax / 180
	y := math.Log(math.Tan(math.Pi/4+lat*math.Pi/360)) * earthRadius
	return XY{x, y}
}

// Line is one row of trace_segments.
type Line struct {
	Points     []XY
	Start, End time.Time // zero when no point has a time
}

// SplitConfig says how to cut a <trkseg> into lines.
type SplitConfig struct {
	MaxPoints  int     // cut after this many points
	MaxLength  float64 // or after this many meters
	LowZooms   []int   // one grid table per zoom, trace_grid_z<N>
	LowMaxJump float64 // the grid edges are cut at edges longer than this, in meters (0: never)
}

// LowTolerance is the size of one pixel at zoom z, for 256 px tiles: the
// cell of the grid of trace_grid_z<z>.
func LowTolerance(z int) float64 {
	return 2 * mercMax / (256 * float64(int(1)<<z))
}

// GridEdge is one edge of trace_grid_z<N>: two nodes of the grid, in cells
// from the origin of Web Mercator. The edge has a direction: a -> b and
// b -> a are two edges.
type GridEdge struct{ X0, Y0, X1, Y1 int32 }

// BuildTrack turns the points of one <trkseg> into:
//   - lines: the rows for trace_segments
//   - grid: per low zoom, the edges for trace_grid_z<zoom> (see GridEdges)
//
// The track is cut at ±180°. Each piece is then cut every MaxPoints points
// or MaxLength meters, and the border point is repeated in the next line.
// Pieces with less than 2 points are dropped.
func BuildTrack(pts []Point, cfg SplitConfig) (lines []Line, grid map[int][]GridEdge) {
	valid := make([]Point, 0, len(pts))
	for _, p := range pts {
		if validPoint(p) {
			valid = append(valid, p)
		}
	}
	for _, piece := range splitDateline(valid) {
		if len(piece) < 2 {
			continue
		}
		lines = append(lines, chunk(piece, cfg.MaxPoints, cfg.MaxLength)...)
	}
	return lines, GridEdges(lines, cfg.LowZooms, cfg.LowMaxJump)
}

// GridEdges turns the lines of one trace into grid edges, per low zoom.
// Each line is cut at GPS jumps longer than maxJump meters (0: never), so
// a jump never shows at low zooms. Every point is moved to the nearest
// node of the grid of one pixel at the zoom, and the edges go between
// the nodes that follow each other. The points are not simplified first:
// two traces on the same road must give the same edges, and a simplify
// would join them in different ways. An edge is returned once per trace,
// so the count in the table is a count of traces.
//
// The lines are the rows of trace_segments, so a delete can build the
// same edges again from the database.
func GridEdges(lines []Line, zooms []int, maxJump float64) map[int][]GridEdge {
	out := make(map[int][]GridEdge, len(zooms))
	for _, z := range zooms {
		cell := LowTolerance(z)
		seen := map[GridEdge]struct{}{}
		edges := []GridEdge{}
		for _, l := range lines {
			for _, part := range splitJumpsXY(l.Points, maxJump) {
				px, py := gridNode(part[0], cell)
				for _, p := range part[1:] {
					x, y := gridNode(p, cell)
					if x == px && y == py {
						continue
					}
					e := GridEdge{px, py, x, y}
					if _, dup := seen[e]; !dup {
						seen[e] = struct{}{}
						edges = append(edges, e)
					}
					px, py = x, y
				}
			}
		}
		out[z] = edges
	}
	return out
}

// gridNode is the nearest grid node of a point.
func gridNode(p XY, cell float64) (int32, int32) {
	return int32(math.Round(p[0] / cell)), int32(math.Round(p[1] / cell))
}

func validPoint(p Point) bool {
	return !math.IsNaN(p.Lat) && !math.IsNaN(p.Lon) &&
		math.Abs(p.Lat) <= 90 && math.Abs(p.Lon) <= 180
}

// splitDateline cuts the track where it crosses ±180°. It adds a point on
// the line at both sides, so each piece ends at the edge of the map.
func splitDateline(pts []Point) [][]Point {
	var out [][]Point
	var cur []Point
	for i, p := range pts {
		if i > 0 {
			a := pts[i-1]
			if math.Abs(p.Lon-a.Lon) > 180 {
				edge, bLon := 180.0, p.Lon+360
				if a.Lon < 0 {
					edge, bLon = -180.0, p.Lon-360
				}
				t := (edge - a.Lon) / (bLon - a.Lon)
				lat := a.Lat + t*(p.Lat-a.Lat)
				cur = append(cur, Point{Lon: edge, Lat: lat})
				out = append(out, cur)
				cur = []Point{{Lon: -edge, Lat: lat}}
			}
		}
		cur = append(cur, p)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// splitJumpsXY cuts Web Mercator points at edges longer than maxJump
// meters on the ground. The edge itself is dropped.
func splitJumpsXY(pts []XY, maxJump float64) [][]XY {
	if maxJump <= 0 {
		return [][]XY{pts}
	}
	var out [][]XY
	start := 0
	for i := 1; i < len(pts); i++ {
		if groundDist(pts[i-1], pts[i]) > maxJump {
			if i-start >= 2 {
				out = append(out, pts[start:i])
			}
			start = i
		}
	}
	if len(pts)-start >= 2 {
		out = append(out, pts[start:])
	}
	return out
}

// groundDist is the distance in meters between two Web Mercator points,
// corrected for the scale of the projection at their latitude.
func groundDist(a, b XY) float64 {
	lat := 2*math.Atan(math.Exp((a[1]+b[1])/2/earthRadius)) - math.Pi/2
	return math.Hypot(b[0]-a[0], b[1]-a[1]) * math.Cos(lat)
}

func chunk(pts []Point, maxPoints int, maxLength float64) []Line {
	if maxPoints < 2 {
		maxPoints = 2
	}
	var lines []Line
	start := 0
	length := 0.0
	for i := 1; i < len(pts); i++ {
		length += haversine(pts[i-1], pts[i])
		if i-start+1 >= maxPoints || (maxLength > 0 && length >= maxLength) {
			lines = append(lines, makeLine(pts[start:i+1]))
			start, length = i, 0
		}
	}
	if len(pts)-start >= 2 {
		lines = append(lines, makeLine(pts[start:]))
	}
	return lines
}

func makeLine(pts []Point) Line {
	l := Line{Points: project(pts)}
	for _, p := range pts {
		if p.Time.IsZero() {
			continue
		}
		if l.Start.IsZero() || p.Time.Before(l.Start) {
			l.Start = p.Time
		}
		if l.End.IsZero() || p.Time.After(l.End) {
			l.End = p.Time
		}
	}
	return l
}

func project(pts []Point) []XY {
	out := make([]XY, len(pts))
	for i, p := range pts {
		out[i] = ToMercator(p.Lon, p.Lat)
	}
	return out
}

// haversine returns the distance in meters between two lon/lat points.
func haversine(a, b Point) float64 {
	const r = 6371000.0
	lat1, lat2 := a.Lat*math.Pi/180, b.Lat*math.Pi/180
	dLat := lat2 - lat1
	dLon := (b.Lon - a.Lon) * math.Pi / 180
	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(h)))
}

// Simplify uses Douglas-Peucker. It always keeps the first and last point.
func Simplify(pts []XY, tol float64) []XY {
	if len(pts) <= 2 || tol <= 0 {
		return pts
	}
	keep := make([]bool, len(pts))
	keep[0], keep[len(pts)-1] = true, true
	stack := [][2]int{{0, len(pts) - 1}}
	for len(stack) > 0 {
		s, e := stack[len(stack)-1][0], stack[len(stack)-1][1]
		stack = stack[:len(stack)-1]
		maxD, idx := 0.0, -1
		for i := s + 1; i < e; i++ {
			if d := segDist(pts[i], pts[s], pts[e]); d > maxD {
				maxD, idx = d, i
			}
		}
		if idx >= 0 && maxD > tol {
			keep[idx] = true
			stack = append(stack, [2]int{s, idx}, [2]int{idx, e})
		}
	}
	out := make([]XY, 0, len(pts))
	for i, k := range keep {
		if k {
			out = append(out, pts[i])
		}
	}
	return out
}

// segDist is the distance from p to the segment a-b.
func segDist(p, a, b XY) float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	if dx == 0 && dy == 0 {
		return math.Hypot(p[0]-a[0], p[1]-a[1])
	}
	t := ((p[0]-a[0])*dx + (p[1]-a[1])*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(p[0]-(a[0]+t*dx), p[1]-(a[1]+t*dy))
}
