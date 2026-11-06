package importer

import (
	"context"
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
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
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
