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

// modeOf is the mode of one segment, like BuildTrack does it.
func modeOf(pts []Point) Mode {
	m, _ := modeOfSpeeds(edgeSpeeds(pts, uploaded), DefaultModeSpeeds())
	return m
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
		if got := modeOf(c.pts); got != c.want {
			t.Errorf("%s: mode = %d, want %d", c.name, got, c.want)
		}
	}
}

// One GPS jump in a walk must not change the mode: the median ignores it.
func TestModeOfIgnoresJump(t *testing.T) {
	t0 := time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)
	pts := pathAt(10, 1.39, time.Second, t0)
	pts[5].Lon += 0.05 // 5 km away, for one second
	if got := modeOf(pts); got != ModeWalk {
		t.Errorf("mode = %d, want walk", got)
	}
}

// A point with the same time as the one before has no speed and is skipped.
func TestModeOfSkipsZeroDuration(t *testing.T) {
	t0 := time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)
	pts := pathAt(10, 25, time.Second, t0)
	pts[4].Time = pts[3].Time
	if got := modeOf(pts); got != ModeVehicle {
		t.Errorf("mode = %d, want vehicle", got)
	}
}

// An edge over 1,200 km/h is a GPS jump: it is not a speed.
func TestEdgeSpeedsLeavesOutJumps(t *testing.T) {
	t0 := time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)
	pts := pathAt(10, 1.39, time.Second, t0)
	if n := len(edgeSpeeds(pts, uploaded)); n != 9 {
		t.Fatalf("%d speeds, want 9", n)
	}
	pts[5].Lon += 0.05 // 5 km in one second: 18,000 km/h, two edges
	if n := len(edgeSpeeds(pts, uploaded)); n != 7 {
		t.Errorf("%d speeds, want 7 (two jump edges left out)", n)
	}
}

// Only jumps and no real edge: not enough speeds, so unknown.
func TestModeOfSpeedsTooFew(t *testing.T) {
	if m, ok := modeOfSpeeds([]float64{1, 2}, DefaultModeSpeeds()); ok || m != ModeUnknown {
		t.Errorf("got %d %v, want unknown false", m, ok)
	}
	if m, ok := modeOfSpeeds([]float64{0.5, 1, 1.5}, DefaultModeSpeeds()); !ok || m != ModeWalk {
		t.Errorf("got %d %v, want walk true", m, ok)
	}
}

func TestParseModeSpeeds(t *testing.T) {
	ms, err := ParseModeSpeeds("7,30,250")
	if err != nil {
		t.Fatal(err)
	}
	if ms != DefaultModeSpeeds() {
		t.Errorf("got %+v, want the defaults", ms)
	}
	if ms.String() != "7,30,250" {
		t.Errorf("String = %q", ms.String())
	}
	for _, bad := range []string{"30,7,250", "7,30", "a,b,c", "", "0,30,250", "7,30,250,300"} {
		if _, err := ParseModeSpeeds(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	ms, err = ParseModeSpeeds(" 5.5, 20, 300 ")
	if err != nil || ms.String() != "5.5,20,300" {
		t.Errorf("got %v, %v", ms, err)
	}
}
