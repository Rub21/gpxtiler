package importer

import (
	"encoding/xml"
	"io"
)

// ParseOriginal reads the track segments of an original GPX file, as the
// website serves it on /traces/:id/data.gpx. It accepts GPX 1.0 and 1.1.
// A tar upload comes as many GPX documents one after the other; all their
// segments are returned, in file order. Waypoints and routes are ignored,
// like the website importer does.
func ParseOriginal(in io.Reader) ([][]Point, error) {
	r := &Reader{dec: xml.NewDecoder(in)}
	r.dec.Strict = false
	var segs [][]Point
	for {
		tok, err := r.dec.Token()
		if err == io.EOF {
			return segs, nil
		}
		if err != nil {
			return segs, err
		}
		e, ok := tok.(xml.StartElement)
		if !ok || e.Name.Local != "trkseg" {
			continue
		}
		seg, err := r.readSeg()
		if err != nil {
			return segs, err
		}
		// Keep empty segments too, so the index is the trackid, like the dump.
		segs = append(segs, seg)
	}
}

// countPoints returns the number of points in all segments.
func countPoints(segs [][]Point) int {
	n := 0
	for _, s := range segs {
		n += len(s)
	}
	return n
}
