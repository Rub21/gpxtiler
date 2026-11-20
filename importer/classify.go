package importer

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Mode is the transport of a <trkseg>, from the median speed of its points.
type Mode int16

const (
	ModeUnknown Mode = 0 // fewer than minTimedEdges edges with a valid time, in the segment and in the trace
	ModeWalk    Mode = 1 // under ModeSpeeds.Walk (default 7 km/h)
	ModeBike    Mode = 2 // under ModeSpeeds.Bike (default 30 km/h)
	ModeVehicle Mode = 3 // under ModeSpeeds.Vehicle (default 250 km/h): cars, buses and trains
	ModePlane   Mode = 4
)

// A line needs this many edges with time to get a mode.
const minTimedEdges = 3

// maxSpeed is 1,200 km/h. An edge faster than this is a GPS jump, not a
// speed: it is left out of the median.
const maxSpeed = 1200.0 / 3.6

// ModeSpeeds are the upper speeds of walk, bike and vehicle in m/s. Above
// Vehicle is plane.
type ModeSpeeds struct{ Walk, Bike, Vehicle float64 }

// DefaultModeSpeeds is 7, 30 and 250 km/h. Trains and cars overlap, so they
// share a mode.
func DefaultModeSpeeds() ModeSpeeds {
	return ModeSpeeds{Walk: 7.0 / 3.6, Bike: 30.0 / 3.6, Vehicle: 250.0 / 3.6}
}

// ParseModeSpeeds reads "7,30,250" (km/h). The three values must be
// positive and increasing.
func ParseModeSpeeds(s string) (ModeSpeeds, error) {
	fields := strings.Split(s, ",")
	if len(fields) != 3 {
		return ModeSpeeds{}, fmt.Errorf("mode speeds must be three km/h values like 7,30,250, got %q", s)
	}
	var v [3]float64
	for i, f := range fields {
		x, err := strconv.ParseFloat(strings.TrimSpace(f), 64)
		if err != nil || math.IsNaN(x) || math.IsInf(x, 0) || x <= 0 {
			return ModeSpeeds{}, fmt.Errorf("bad mode speed %q: it must be a positive number", strings.TrimSpace(f))
		}
		v[i] = x / 3.6
	}
	if !(v[0] < v[1] && v[1] < v[2]) {
		return ModeSpeeds{}, fmt.Errorf("mode speeds must be increasing, got %q", s)
	}
	return ModeSpeeds{Walk: v[0], Bike: v[1], Vehicle: v[2]}, nil
}

// String gives the km/h form back, "7,30,250", for the flag help and dump_state.
func (m ModeSpeeds) String() string {
	f := func(v float64) string { return strconv.FormatFloat(math.Round(v*3.6*1e6)/1e6, 'f', -1, 64) }
	return f(m.Walk) + "," + f(m.Bike) + "," + f(m.Vehicle)
}

// The first year with GPS traces worth keeping. Older times are GPS clocks
// that were never set (1970, 1980).
var minTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// validTime tells whether a GPS time can be right: not before 2000, and not
// after the upload (one day of slack for time zones and bad clocks).
func validTime(t, uploaded time.Time) bool {
	return !t.IsZero() && !t.Before(minTime) && !t.After(uploaded.Add(24*time.Hour))
}

// YearOf is the year of a line: the GPS time of its first timed point when
// that time is valid, else the upload time (no timed point, or a first time
// before 2000 or after the upload).
func YearOf(pts []Point, uploaded time.Time) int16 {
	for _, p := range pts {
		if p.Time.IsZero() {
			continue
		}
		if validTime(p.Time, uploaded) {
			return int16(p.Time.UTC().Year())
		}
		break // the first time is bad: do not trust the others
	}
	return int16(uploaded.UTC().Year())
}

// edgeSpeeds returns the speed of every edge with a valid time at both
// ends, a positive duration and a possible speed (under maxSpeed). The
// rest are GPS jumps or clock errors and are left out.
func edgeSpeeds(pts []Point, uploaded time.Time) []float64 {
	speeds := make([]float64, 0, len(pts))
	for i := 1; i < len(pts); i++ {
		a, b := pts[i-1], pts[i]
		if !validTime(a.Time, uploaded) || !validTime(b.Time, uploaded) {
			continue
		}
		dt := b.Time.Sub(a.Time).Seconds()
		if dt <= 0 {
			continue
		}
		if v := haversine(a, b) / dt; v < maxSpeed {
			speeds = append(speeds, v)
		}
	}
	return speeds
}

// modeOfSpeeds classifies the median of speeds. The median, not the mean,
// so one GPS jump does not turn a walk into a flight. ok is false with
// fewer than minTimedEdges speeds.
func modeOfSpeeds(speeds []float64, ms ModeSpeeds) (mode Mode, ok bool) {
	if len(speeds) < minTimedEdges {
		return ModeUnknown, false
	}
	s := slices.Clone(speeds)
	slices.Sort(s)
	v := s[len(s)/2] // upper middle for an even count: fine for classes
	switch {
	case v < ms.Walk:
		return ModeWalk, true
	case v < ms.Bike:
		return ModeBike, true
	case v < ms.Vehicle:
		return ModeVehicle, true
	default:
		return ModePlane, true
	}
}
