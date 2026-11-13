package importer

import (
	"slices"
	"time"
)

// Mode is the transport of one line, from the median speed of its points.
type Mode int16

const (
	ModeUnknown Mode = 0 // fewer than minTimedEdges edges with a valid time
	ModeWalk    Mode = 1 // under 7 km/h
	ModeBike    Mode = 2 // under 30 km/h
	ModeVehicle Mode = 3 // under 250 km/h: cars, buses and trains
	ModePlane   Mode = 4
)

// Speed limits in m/s. Trains and cars overlap, so they share a mode.
const (
	walkMax    = 7.0 / 3.6
	bikeMax    = 30.0 / 3.6
	vehicleMax = 250.0 / 3.6
	// A line needs this many edges with time to get a mode.
	minTimedEdges = 3
)

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

// ModeOf classifies a line by the median speed of its edges. The median,
// not the mean, so one GPS jump does not turn a walk into a flight.
func ModeOf(pts []Point, uploaded time.Time) Mode {
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
		speeds = append(speeds, haversine(a, b)/dt)
	}
	if len(speeds) < minTimedEdges {
		return ModeUnknown
	}
	slices.Sort(speeds)
	v := speeds[len(speeds)/2] // upper middle for an even count: fine for classes
	switch {
	case v < walkMax:
		return ModeWalk
	case v < bikeMax:
		return ModeBike
	case v < vehicleMax:
		return ModeVehicle
	default:
		return ModePlane
	}
}
