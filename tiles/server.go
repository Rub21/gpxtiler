package tiles

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"github.com/Rub21/gpxtiler/importer"
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
	// TraceMaxPoints: /traces/{id} answers 413 for a trace with more points,
	// so one request cannot make the database read millions of points.
	// 0 turns /traces/{id} off.
	TraceMaxPoints int
	Log            *log.Logger

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
	if s.TraceMaxPoints > 0 {
		mux.HandleFunc("GET /traces/{id}", s.trace)
	}
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

// trace answers /traces/{id} with the trace as a GeoJSON Feature: the same
// lines and points as the GPX file, one line per <trkseg>, with the altitude
// as the third coordinate. The viewer draws it and shows what was loaded.
// 404 when the trace is not in the database, 413 when it has more points
// than TraceMaxPoints.
func (s *Server) trace(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad trace id", http.StatusNotFound)
		return
	}
	var name *string
	var points int64
	// Count the points first. It only reads the start of each row, so it is
	// cheap also for the biggest trace.
	err = s.Renderer.Pool.QueryRow(r.Context(), `
		SELECT t.name, coalesce(sum(ST_NPoints(s.geom)), 0)
		FROM traces t
		LEFT JOIN trace_segments s ON s.gpx_id = t.gpx_id
		WHERE t.gpx_id = $1
		GROUP BY t.gpx_id, t.name`, id).Scan(&name, &points)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "trace not loaded", http.StatusNotFound)
		return
	}
	if err != nil {
		s.Log.Printf("trace %d: %v", id, err)
		http.Error(w, "database error", http.StatusServiceUnavailable)
		return
	}
	if points > int64(s.TraceMaxPoints) {
		http.Error(w, fmt.Sprintf("trace %d has %d points, the limit is %d", id, points, s.TraceMaxPoints), http.StatusRequestEntityTooLarge)
		return
	}
	rows, err := s.Renderer.Pool.Query(r.Context(), `
		SELECT trackid, ST_AsEWKB(ST_Transform(geom, 4326))
		FROM trace_segments
		WHERE gpx_id = $1
		ORDER BY trackid, segment`, id)
	if err != nil {
		s.Log.Printf("trace %d: %v", id, err)
		http.Error(w, "database error", http.StatusServiceUnavailable)
		return
	}
	// Import cuts a <trkseg> in several rows (every 1000 points or 50 km),
	// and the next row starts at the last point of the one before. Here the
	// rows are joined again, so the answer has one line per <trkseg>, like
	// the GPX file. A row that does not start where the one before ends is
	// a new line: the <trkseg> was cut at ±180°.
	var coords []byte // the lines, as GeoJSON coordinates
	var segments, lastTrack int
	var last [3]float64
	var trackid int
	var wkb []byte
	points = 0
	_, err = pgx.ForEachRow(rows, []any{&trackid, &wkb}, func() error {
		pts, ele, err := importer.ParseWKBLineStringZ(wkb)
		if err != nil || len(pts) == 0 {
			return err
		}
		first := 0
		if segments > 0 && trackid == lastTrack && last == [3]float64{pts[0][0], pts[0][1], ele[0]} {
			first = 1 // the point of the cut is already there
			coords = coords[:len(coords)-1]
		} else {
			if segments > 0 {
				coords = append(coords, ',')
			}
			coords = append(coords, '[')
			segments++
		}
		for i := first; i < len(pts); i++ {
			if i > 0 || first > 0 {
				coords = append(coords, ',')
			}
			coords = appendCoord(coords, pts[i], ele[i])
		}
		coords = append(coords, ']')
		points += int64(len(pts) - first)
		lastTrack = trackid
		n := len(pts) - 1
		last = [3]float64{pts[n][0], pts[n][1], ele[n]}
		return nil
	})
	if err != nil {
		s.Log.Printf("trace %d: %v", id, err)
		http.Error(w, "database error", http.StatusServiceUnavailable)
		return
	}
	feature := struct {
		Type       string          `json:"type"`
		Properties map[string]any  `json:"properties"`
		Geometry   json.RawMessage `json:"geometry"`
	}{
		Type:       "Feature",
		Properties: map[string]any{"id": id, "name": name, "segments": segments, "points": points},
		Geometry:   json.RawMessage("null"),
	}
	if segments > 0 {
		feature.Geometry = json.RawMessage(`{"type":"MultiLineString","coordinates":[` + string(coords) + `]}`)
	}
	w.Header().Set("Content-Type", "application/geo+json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(feature)
}

// appendCoord writes one point as [lon,lat,ele], with the decimals of a GPX
// file of OSM: 7 for lon and lat, 2 for the altitude. The altitude is stored
// on a grid of 1/128 m (9.3 m is 9.296875), so it is rounded back.
func appendCoord(b []byte, p importer.XY, ele float64) []byte {
	b = append(b, '[')
	b = strconv.AppendFloat(b, math.Round(p[0]*1e7)/1e7, 'f', -1, 64)
	b = append(b, ',')
	b = strconv.AppendFloat(b, math.Round(p[1]*1e7)/1e7, 'f', -1, 64)
	b = append(b, ',')
	b = strconv.AppendFloat(b, math.Round(ele*100)/100, 'f', -1, 64)
	return append(b, ']')
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
