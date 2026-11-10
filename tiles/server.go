package tiles

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
)

//go:embed viewer.html
var viewerHTML []byte

// Server serves the PNG tiles on /lines/{z}/{x}/{y}.png, the same URL as
// gps-tile.openstreetmap.org, so JOSM and iD keep working.
type Server struct {
	Renderer *Renderer
	// A tile is read from TileDir when it is there, or drawn. Tiles up to
	// CacheZoom are kept in TileDir after they are drawn, so the next
	// request reads them; "render" writes the low zooms there in advance
	// and removes or redraws the expired ones. Above CacheZoom a tile is
	// drawn on every request.
	CacheZoom int
	MaxZoom   int
	TileDir   string        // "" when there are no tiles on disk
	MaxAge    time.Duration // Cache-Control max-age
	Timeout   time.Duration // stop drawing a tile after this time
	Log       *log.Logger

	sem    chan struct{}      // limits the tiles drawn at the same time
	single singleflight.Group // one draw per tile when many ask for it
	empty  []byte             // transparent tile
}

// Handler returns the HTTP handler. maxRenders is the number of tiles drawn
// at the same time; other requests wait.
func (s *Server) Handler(maxRenders int) http.Handler {
	s.sem = make(chan struct{}, maxRenders)
	var buf bytes.Buffer
	png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, TileSize, TileSize)))
	s.empty = buf.Bytes()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /lines/{z}/{x}/{y}", s.tile)
	// A small map to look at the tiles.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(viewerHTML)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Renderer.Pool.Ping(r.Context()); err != nil {
			http.Error(w, "database: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	return mux
}

// parseTile reads z, x and y from the path. y can end in ".png".
func (s *Server) parseTile(r *http.Request) (z, x, y int, err error) {
	z, err1 := strconv.Atoi(r.PathValue("z"))
	x, err2 := strconv.Atoi(r.PathValue("x"))
	y, err3 := strconv.Atoi(strings.TrimSuffix(r.PathValue("y"), ".png"))
	if err := errors.Join(err1, err2, err3); err != nil {
		return 0, 0, 0, errors.New("bad tile path")
	}
	if z < 0 || z > s.MaxZoom {
		return 0, 0, 0, fmt.Errorf("zoom must be 0 to %d", s.MaxZoom)
	}
	n := 1 << z
	if x < 0 || x >= n || y < 0 || y >= n {
		return 0, 0, 0, errors.New("tile out of range")
	}
	return z, x, y, nil
}

func (s *Server) tile(w http.ResponseWriter, r *http.Request) {
	// Same as the Apache config of gps-tile, so CORS requests can be cached.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	z, x, y, err := s.parseTile(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	var data []byte
	onDisk := s.TileDir != "" && z <= s.CacheZoom
	if onDisk {
		data = ReadTile(s.TileDir, z, x, y)
	}
	if data == nil {
		data, err = s.draw(r.Context(), z, x, y)
		if err != nil {
			if r.Context().Err() == nil {
				s.Log.Printf("tile %d/%d/%d: %v", z, x, y, err)
			}
			http.Error(w, "could not draw the tile", http.StatusServiceUnavailable)
			return
		}
		// Keep it for the next request. Empty tiles are cheap to draw
		// again, so they are not stored.
		if onDisk && data != nil {
			if err := WriteTile(s.TileDir, z, x, y, data); err != nil {
				s.Log.Printf("cache %d/%d/%d: %v", z, x, y, err)
			}
		}
	}
	if data == nil {
		data = s.empty
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(s.MaxAge.Seconds())))
	w.Write(data)
}

// draw renders a tile, waiting for a free slot. When the same tile is
// asked for by many requests at once (a low zoom tile not on disk yet),
// it is drawn once and they all get that result.
func (s *Server) draw(ctx context.Context, z, x, y int) ([]byte, error) {
	key := fmt.Sprintf("%d/%d/%d", z, x, y)
	v, err, _ := s.single.Do(key, func() (any, error) {
		ctx, cancel := context.WithTimeout(ctx, s.Timeout)
		defer cancel()
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		defer func() { <-s.sem }()
		return s.Renderer.Render(ctx, z, x, y)
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}
