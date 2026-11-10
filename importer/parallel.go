package importer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"
)

// The dump is read in a pipeline, so the XML parsing (the slow part) uses
// all the cores:
//
//	splitter: cuts the stream into <trk>...</trk> chunks, by bytes
//	workers:  parse a chunk and build the rows of its lines
//	Run:      writes the rows to the database, in the order they come
//
// The chunks need the <gpx> start tag in front of them to parse, for the
// namespaces, so the header is read first.

// dumpStream is the dump after its header.
type dumpStream struct {
	gpxStart []byte // the <gpx ...> start tag
	r        io.Reader
	buf      []byte // data read, not yet returned
	scanned  int    // buf[:scanned] has no </trk>
	eof      bool
}

// openDump reads the dump header, up to the first <trk>, and returns the
// dump time and the stream of traces.
func openDump(in io.Reader) (*Reader, *dumpStream, error) {
	br := bufio.NewReaderSize(in, 1<<20)
	var header []byte
	for {
		part, err := br.ReadSlice('<')
		header = append(header, part...)
		if err == io.EOF {
			break
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return nil, nil, err
		}
		if next, _ := br.Peek(4); bytes.HasPrefix(next, []byte("trk>")) || bytes.HasPrefix(next, []byte("trk ")) {
			header = header[:len(header)-1] // the '<' of <trk> stays in the stream
			break
		}
	}
	// A complete small document, to read the <time> the usual way.
	rd, err := NewReader(io.MultiReader(bytes.NewReader(header), strings.NewReader("</gpx>")))
	if err != nil {
		return nil, nil, err
	}
	i := bytes.Index(header, []byte("<gpx"))
	j := bytes.IndexByte(header[i:], '>')
	if i < 0 || j < 0 {
		return nil, nil, errors.New("no <gpx> element")
	}
	ds := &dumpStream{gpxStart: header[i : i+j+1], r: br, buf: []byte("<")}
	return rd, ds, nil
}

const trkEnd = "</trk>"

// next returns the next <trk>...</trk> chunk, or io.EOF. It only scans
// each byte once: the search goes on where the last one stopped.
func (ds *dumpStream) next() ([]byte, error) {
	for {
		if i := bytes.Index(ds.buf[ds.scanned:], []byte(trkEnd)); i >= 0 {
			end := ds.scanned + i + len(trkEnd)
			chunk := bytes.Clone(ds.buf[:end])
			ds.buf = ds.buf[end:]
			ds.scanned = 0
			if !bytes.Contains(chunk, []byte("<trk")) {
				return nil, errors.New("reading dump: </trk> without <trk>")
			}
			return chunk, nil
		}
		if ds.eof {
			return nil, io.EOF // the tail after the last trace: </gpx>
		}
		ds.scanned = max(0, len(ds.buf)-len(trkEnd)+1)
		if len(ds.buf) == cap(ds.buf) {
			ds.buf = slices.Grow(ds.buf, max(1<<20, len(ds.buf)))
		}
		n, err := ds.r.Read(ds.buf[len(ds.buf):cap(ds.buf)])
		ds.buf = ds.buf[:len(ds.buf)+n]
		if err == io.EOF {
			ds.eof = true
		} else if err != nil {
			return nil, err
		}
	}
}

// parseChunk parses one <trk>...</trk> chunk.
func parseChunk(gpxStart, chunk []byte, skipGeom func(int64) bool) (*Trace, error) {
	dec := xml.NewDecoder(io.MultiReader(bytes.NewReader(gpxStart), bytes.NewReader(chunk), strings.NewReader("</gpx>")))
	r := &Reader{dec: dec}
	if _, err := dec.Token(); err != nil { // <gpx>
		return nil, err
	}
	if err := r.findTrk(); err != nil {
		return nil, err
	}
	return r.readTrk(skipGeom)
}

// parsed is one trace with its rows ready for COPY.
type parsed struct {
	t       *Trace
	segRows []byte         // trace_segments rows
	lowRows map[int][]byte // per low zoom, trace_segments_z<N> rows
	lines   []Line         // for the expire file
	n       int            // segments
}

// buildRows makes the rows of the lines of a trace.
func buildRows(t *Trace, cfg SplitConfig) *parsed {
	p := &parsed{t: t}
	var seg bytes.Buffer
	low := map[int]*bytes.Buffer{}
	for trackid, pts := range t.Segments {
		lines, lowParts := BuildTrack(pts, cfg)
		for i, l := range lines {
			fmt.Fprintf(&seg, "%d\t%d\t%d\t%s\t%s\t%s\n", t.ID, trackid, i,
				hex.EncodeToString(EWKBLineString(l.Points)), copyTime(l.Start), copyTime(l.End))
			p.n++
		}
		p.lines = append(p.lines, lines...)
		for z, parts := range lowParts {
			if low[z] == nil {
				low[z] = &bytes.Buffer{}
			}
			fmt.Fprintf(low[z], "%d\t%d\t%s\n", t.ID, trackid, hex.EncodeToString(EWKBMultiLineString(parts)))
		}
	}
	p.segRows = seg.Bytes()
	if len(low) > 0 {
		p.lowRows = map[int][]byte{}
		for z, b := range low {
			p.lowRows[z] = b.Bytes()
		}
	}
	return p
}

// parseAll reads the traces of ds with workers goroutines and sends them
// to out, in any order. It closes out when the dump is done.
func parseAll(ctx context.Context, ds *dumpStream, workers int, skipGeom func(int64) bool, cfg SplitConfig, out chan<- *parsed) error {
	g, ctx := errgroup.WithContext(ctx)
	chunks := make(chan []byte, workers)
	g.Go(func() error {
		defer close(chunks)
		for {
			chunk, err := ds.next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return fmt.Errorf("reading dump: %w", err)
			}
			select {
			case chunks <- chunk:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
	for range max(1, workers) {
		g.Go(func() error {
			for chunk := range chunks {
				t, err := parseChunk(ds.gpxStart, chunk, skipGeom)
				if err != nil {
					return fmt.Errorf("reading dump: %w", err)
				}
				select {
				case out <- buildRows(t, cfg):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
	}
	err := g.Wait()
	close(out)
	return err
}
