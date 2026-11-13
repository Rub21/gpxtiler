package importer

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrNotFound means the website has no public data for the trace: it was
// deleted, it is not identifiable, or the id was never used.
var ErrNotFound = errors.New("trace not found")

// Fetcher downloads traces from the website.
type Fetcher struct {
	Site      string // for example https://www.openstreetmap.org
	Client    *http.Client
	UserAgent string
	Delay     time.Duration // pause before each request, to be gentle with the website
}

// NewFetcher returns a Fetcher with a 2 minute timeout, like gpx-updater.
func NewFetcher(site string) *Fetcher {
	return &Fetcher{
		Site:      strings.TrimRight(site, "/"),
		Client:    &http.Client{Timeout: 2 * time.Minute},
		UserAgent: "gpxtiler (+https://github.com/Rub21/gpxtiler)",
		Delay:     500 * time.Millisecond,
	}
}

// get runs a GET request and returns the body. The caller closes it.
func (f *Fetcher) get(ctx context.Context, path string) (io.ReadCloser, error) {
	if f.Delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.Delay):
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.Site+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", f.UserAgent)
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return resp.Body, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusForbidden:
		resp.Body.Close()
		return nil, ErrNotFound
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", path, resp.Status)
	}
}

// Trace downloads the original file of one trace and parses its segments.
// The website only serves it for public and identifiable traces, and it
// already unpacks gz, bz2, zip and tar uploads for the .gpx format.
func (f *Fetcher) Trace(ctx context.Context, id int64) ([][]Point, error) {
	body, err := f.get(ctx, fmt.Sprintf("/traces/%d/data.gpx", id))
	if err != nil {
		return nil, err
	}
	defer body.Close()
	segs, err := ParseOriginal(body)
	if err != nil {
		return nil, fmt.Errorf("trace %d: %w", id, err)
	}
	return segs, nil
}

// Details reads the upload time of a trace from the API. The website
// answers 403 for traces that are not public: that is ErrNotFound too.
func (f *Fetcher) Details(ctx context.Context, id int64) (time.Time, error) {
	body, err := f.get(ctx, fmt.Sprintf("/api/0.6/gpx/%d/details", id))
	if err != nil {
		return time.Time{}, err
	}
	defer body.Close()
	// <osm><gpx_file timestamp="..."/></osm>
	var doc struct {
		File struct {
			Timestamp string `xml:"timestamp,attr"`
		} `xml:"gpx_file"`
	}
	if err := xml.NewDecoder(body).Decode(&doc); err != nil {
		return time.Time{}, fmt.Errorf("trace %d details: %w", id, err)
	}
	t, err := time.Parse(time.RFC3339, doc.File.Timestamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("trace %d details: bad timestamp %q: %w", id, doc.File.Timestamp, err)
	}
	return t, nil
}

// uploadTime returns the upload time to store with a downloaded trace:
// the feed date when the id is in fromFeed; now when every segment has a
// valid GPS time (the year comes from the GPS, now is only the upper
// bound); else one more request to Details.
func (f *Fetcher) uploadTime(ctx context.Context, id int64, segs [][]Point, fromFeed map[int64]time.Time) (time.Time, error) {
	if t, ok := fromFeed[id]; ok {
		return t, nil
	}
	now := time.Now()
	if !needsUpload(segs, now) {
		return now, nil
	}
	return f.Details(ctx, id)
}
