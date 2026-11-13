package importer

import (
	"testing"
	"time"
)

var uploaded = time.Date(2020, 6, 1, 12, 0, 0, 0, time.UTC)

// pathAt makes n points along the equator, step meters apart, with one
// point every `every` seconds. The first point is at t0.
func pathAt(n int, step float64, every time.Duration, t0 time.Time) []Point {
	const mPerDeg = 111320.0
	pts := make([]Point, n)
	for i := range pts {
		pts[i] = Point{Lon: float64(i) * step / mPerDeg}
		if !t0.IsZero() {
			pts[i].Time = t0.Add(time.Duration(i) * every)
		}
	}
	return pts
}

func TestYearOf(t *testing.T) {
	t0 := time.Date(2019, 12, 31, 23, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		pts  []Point
		want int16
	}{
		{"gps time", pathAt(3, 10, time.Second, t0), 2019},
		{"no time", pathAt(3, 10, time.Second, time.Time{}), 2020},
		{"1970", pathAt(3, 10, time.Second, time.Unix(0, 0).UTC()), 2020},
		{"before 2000", pathAt(3, 10, time.Second, time.Date(1999, 12, 31, 0, 0, 0, 0, time.UTC)), 2020},
		{"future", pathAt(3, 10, time.Second, uploaded.Add(48*time.Hour)), 2020},
		{"same day as upload", pathAt(3, 10, time.Second, uploaded.Add(2*time.Hour)), 2020},
		{"first point without time", append([]Point{{Lon: 0}}, pathAt(2, 10, time.Second, t0)...), 2019},
	}
	for _, c := range cases {
		if got := YearOf(c.pts, uploaded); got != c.want {
			t.Errorf("%s: year = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestModeOf(t *testing.T) {
	t0 := time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		pts  []Point
		want Mode
	}{
		{"walk 5 km/h", pathAt(10, 1.39, time.Second, t0), ModeWalk},
		{"bike 20 km/h", pathAt(10, 5.56, time.Second, t0), ModeBike},
		{"car 90 km/h", pathAt(10, 25, time.Second, t0), ModeVehicle},
		{"train 200 km/h", pathAt(10, 55.6, time.Second, t0), ModeVehicle},
		{"plane 800 km/h", pathAt(10, 222, time.Second, t0), ModePlane},
		{"no time", pathAt(10, 25, time.Second, time.Time{}), ModeUnknown},
		{"two edges only", pathAt(3, 25, time.Second, t0), ModeUnknown},
		{"bad time", pathAt(10, 25, time.Second, time.Unix(0, 0).UTC()), ModeUnknown},
	}
	for _, c := range cases {
		if got := ModeOf(c.pts, uploaded); got != c.want {
			t.Errorf("%s: mode = %d, want %d", c.name, got, c.want)
		}
	}
}

// One GPS jump in a walk must not change the mode: the median ignores it.
func TestModeOfIgnoresJump(t *testing.T) {
	t0 := time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)
	pts := pathAt(10, 1.39, time.Second, t0)
	pts[5].Lon += 0.05 // 5 km away, for one second
	if got := ModeOf(pts, uploaded); got != ModeWalk {
		t.Errorf("mode = %d, want walk", got)
	}
}

// A point with the same time as the one before has no speed and is skipped.
func TestModeOfSkipsZeroDuration(t *testing.T) {
	t0 := time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)
	pts := pathAt(10, 25, time.Second, t0)
	pts[4].Time = pts[3].Time
	if got := ModeOf(pts, uploaded); got != ModeVehicle {
		t.Errorf("mode = %d, want vehicle", got)
	}
}
