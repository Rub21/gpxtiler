package importer

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Namespace of the osm:id and osm:tag elements in the planet-dump-ng dump.
const osmNS = "https://www.openstreetmap.org/gpx/0.1"

// Point is one <trkpt>. Time is zero when the point has no <time>.
type Point struct {
	Lon, Lat float64
	Time     time.Time
}

// Trace is one <trk> of the dump.
type Trace struct {
	ID          int64
	Name        *string // nil when there is no <name> (trackable traces)
	Description *string
	Tags        []string
	// One entry per <trkseg>, in dump order. The index is the trackid.
	// Empty when the geometry was skipped.
	Segments [][]Point
}

// Reader reads the GPX dump as a stream, one trace at a time.
type Reader struct {
	dec      *xml.Decoder
	DumpTime time.Time // <time> in <gpx>
	inTrk    bool      // the header loop already read a <trk> start
}

// NewReader reads the dump header and stops at the first <trk>.
func NewReader(r io.Reader) (*Reader, error) {
	rd := &Reader{dec: xml.NewDecoder(r)}
	if err := rd.readHeader(); err != nil {
		return nil, err
	}
	if rd.DumpTime.IsZero() {
		return nil, errors.New("dump has no <time> in <gpx>")
	}
	return rd, nil
}

func (r *Reader) readHeader() error {
	sawGpx := false
	for {
		tok, err := r.dec.Token()
		if err == io.EOF {
			if !sawGpx {
				return errors.New("no <gpx> element")
			}
			return nil
		}
		if err != nil {
			return err
		}
		switch e := tok.(type) {
		case xml.StartElement:
			if !sawGpx {
				if e.Name.Local != "gpx" {
					return fmt.Errorf("expected <gpx>, got <%s>", e.Name.Local)
				}
				sawGpx = true
				continue
			}
			switch e.Name.Local {
			case "time":
				s, err := readText(r.dec)
				if err != nil {
					return err
				}
				r.DumpTime, err = time.Parse(time.RFC3339, strings.TrimSpace(s))
				if err != nil {
					return fmt.Errorf("bad dump <time>: %w", err)
				}
			case "trk":
				r.inTrk = true
				return nil
			default:
				if err := r.dec.Skip(); err != nil {
					return err
				}
			}
		case xml.EndElement: // </gpx> without traces
			return nil
		}
	}
}

// Next returns the next trace, or io.EOF at the end of the dump.
// When skipGeom returns true for the trace id, the <trkseg> elements are
// not parsed and Segments stays empty.
func (r *Reader) Next(skipGeom func(id int64) bool) (*Trace, error) {
	if !r.inTrk {
		if err := r.findTrk(); err != nil {
			return nil, err
		}
	}
	r.inTrk = false
	return r.readTrk(skipGeom)
}

func (r *Reader) findTrk() error {
	for {
		tok, err := r.dec.Token()
		if err != nil {
			return err // io.EOF at the end of the file
		}
		switch e := tok.(type) {
		case xml.StartElement:
			if e.Name.Local == "trk" {
				return nil
			}
			if err := r.dec.Skip(); err != nil {
				return err
			}
		case xml.EndElement: // </gpx>
			return io.EOF
		}
	}
}

func (r *Reader) readTrk(skipGeom func(id int64) bool) (*Trace, error) {
	t := &Trace{}
	hasID := false
	skip := false
	for {
		tok, err := r.dec.Token()
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		if err != nil {
			return nil, err
		}
		switch e := tok.(type) {
		case xml.StartElement:
			osm := e.Name.Space == osmNS
			switch {
			case osm && e.Name.Local == "id":
				s, err := readText(r.dec)
				if err != nil {
					return nil, err
				}
				t.ID, err = strconv.ParseInt(strings.TrimSpace(s), 10, 64)
				if err != nil {
					return nil, fmt.Errorf("bad <osm:id> %q: %w", s, err)
				}
				hasID = true
				skip = skipGeom != nil && skipGeom(t.ID)
			case osm && e.Name.Local == "tag":
				s, err := readText(r.dec)
				if err != nil {
					return nil, err
				}
				t.Tags = append(t.Tags, s)
			case !osm && e.Name.Local == "name":
				s, err := readText(r.dec)
				if err != nil {
					return nil, err
				}
				t.Name = &s
			case !osm && e.Name.Local == "desc":
				s, err := readText(r.dec)
				if err != nil {
					return nil, err
				}
				t.Description = &s
			case !osm && e.Name.Local == "trkseg":
				if !hasID {
					return nil, errors.New("<trkseg> before <osm:id>")
				}
				if skip {
					if err := r.dec.Skip(); err != nil {
						return nil, err
					}
					continue
				}
				seg, err := r.readSeg()
				if err != nil {
					return nil, err
				}
				// Keep empty segments too, so the index stays the trackid.
				t.Segments = append(t.Segments, seg)
			default:
				if err := r.dec.Skip(); err != nil {
					return nil, err
				}
			}
		case xml.EndElement: // </trk>
			if !hasID {
				return nil, errors.New("<trk> without <osm:id>")
			}
			return t, nil
		}
	}
}

func (r *Reader) readSeg() ([]Point, error) {
	var pts []Point
	for {
		tok, err := r.dec.Token()
		if err != nil {
			return nil, err
		}
		switch e := tok.(type) {
		case xml.StartElement:
			if e.Name.Local != "trkpt" {
				if err := r.dec.Skip(); err != nil {
					return nil, err
				}
				continue
			}
			p, ok, err := r.readPoint(e)
			if err != nil {
				return nil, err
			}
			if ok {
				pts = append(pts, p)
			}
		case xml.EndElement: // </trkseg>
			return pts, nil
		}
	}
}

// readPoint reads one <trkpt>. ok is false when lat or lon is missing or bad.
func (r *Reader) readPoint(start xml.StartElement) (Point, bool, error) {
	var p Point
	var errLat, errLon error = errors.New("missing"), errors.New("missing")
	for _, a := range start.Attr {
		switch a.Name.Local {
		case "lat":
			p.Lat, errLat = strconv.ParseFloat(a.Value, 64)
		case "lon":
			p.Lon, errLon = strconv.ParseFloat(a.Value, 64)
		}
	}
	for {
		tok, err := r.dec.Token()
		if err != nil {
			return p, false, err
		}
		switch e := tok.(type) {
		case xml.StartElement:
			if e.Name.Local == "time" {
				s, err := readText(r.dec)
				if err != nil {
					return p, false, err
				}
				// A bad time is ignored, the point is still used.
				if t, err := time.Parse(time.RFC3339, strings.TrimSpace(s)); err == nil {
					p.Time = t
				}
				continue
			}
			if err := r.dec.Skip(); err != nil {
				return p, false, err
			}
		case xml.EndElement: // </trkpt>
			return p, errLat == nil && errLon == nil, nil
		}
	}
}

// readText returns the text of the current element and reads its end tag.
func readText(dec *xml.Decoder) (string, error) {
	var b strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch e := tok.(type) {
		case xml.CharData:
			b.Write(e)
		case xml.StartElement:
			if err := dec.Skip(); err != nil {
				return "", err
			}
		case xml.EndElement:
			return b.String(), nil
		}
	}
}
