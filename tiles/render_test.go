package tiles

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Rub21/gpxtiler/importer"
	"github.com/Rub21/gpxtiler/schema"
)

// A trace in Vaduz going east, loaded with import.
const vaduz = `<?xml version="1.0" encoding="UTF-8"?>
<gpx version="1.0" xmlns="http://www.topografix.com/GPX/1/0" xmlns:osm="https://www.openstreetmap.org/gpx/0.1">
 <time>2026-10-31T00:00:00Z</time>
 <trk><osm:id>1</osm:id><trkseg>
  <trkpt lat="47.1400" lon="9.5200"/><trkpt lat="47.1400" lon="9.5210"/><trkpt lat="47.1400" lon="9.5220"/>
 </trkseg></trk>
</gpx>
`

// testPool loads the Vaduz trace in a clean schema and returns a pool on it.
// The test is skipped when GPXTILER_TEST_DB is not set.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("GPXTILER_TEST_DB")
	if dsn == "" {
		t.Skip("GPXTILER_TEST_DB is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	_, err = conn.PgConn().Exec(ctx, `
		CREATE EXTENSION IF NOT EXISTS postgis;
		DROP SCHEMA IF EXISTS gpxtiler_tiles_test CASCADE;
		CREATE SCHEMA gpxtiler_tiles_test;
		SET search_path TO gpxtiler_tiles_test, public`).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Apply(ctx, conn); err != nil {
		t.Fatal(err)
	}
	cfg := importer.DefaultConfig()
	cfg.Log = log.New(io.Discard, "", 0)
	if _, err := importer.Run(ctx, conn, strings.NewReader(vaduz), cfg); err != nil {
		t.Fatal(err)
	}

	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pcfg.ConnConfig.RuntimeParams["search_path"] = "gpxtiler_tiles_test, public"
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Tiles with the trace, from zoom 16 in Vaduz.
const z16x, z16y = 34501, 23013

func TestRender(t *testing.T) {
	pool := testPool(t)
	r := &Renderer{Pool: pool, LowZoom: 8, Colors: 64}
	ctx := context.Background()

	data, err := r.Render(ctx, 16, z16x, z16y)
	if err != nil {
		t.Fatal(err)
	}
	if data == nil {
		t.Fatal("tile with the trace is empty")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := img.(*image.Paletted); !ok {
		t.Errorf("tile is %T, want a paletted PNG", img)
	} else if len(p.Palette) > 64 {
		t.Errorf("palette has %d colors, want at most 64", len(p.Palette))
	}
	// Full color when Colors is 0.
	full, err := (&Renderer{Pool: pool, LowZoom: 8}).Render(ctx, 16, z16x, z16y)
	if err != nil || len(full) <= len(data) {
		t.Errorf("full color tile: %d bytes, paletted %d bytes, %v", len(full), len(data), err)
	}
	// The trace goes east: drawn pixels are red.
	red := 0
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, _, a := img.At(x, y).RGBA()
			if a > 0 && r > g {
				red++
			}
		}
	}
	if red == 0 {
		t.Error("no red pixels for a trace going east")
	}

	// A tile far away is empty.
	if data, err := r.Render(ctx, 16, 0, 0); err != nil || data != nil {
		t.Errorf("far tile: %d bytes, %v", len(data), err)
	}
	// Low zoom reads trace_segments_low.
	if data, err := r.Render(ctx, 8, 134, 89); err != nil || data == nil {
		t.Errorf("low zoom tile: %d bytes, %v", len(data), err)
	}
}

func TestServer(t *testing.T) {
	pool := testPool(t)
	dir := t.TempDir()
	// A low zoom tile written by render.
	os.MkdirAll(filepath.Join(dir, "2", "2"), 0o755)
	os.WriteFile(TilePath(dir, 2, 2, 1), []byte("disk tile"), 0o644)

	s := &Server{
		Renderer:    &Renderer{Pool: pool, LowZoom: 8},
		MinLiveZoom: 6,
		MaxZoom:     20,
		TileDir:     dir,
		MaxAge:      time.Hour,
		Timeout:     10 * time.Second,
		Log:         log.New(io.Discard, "", 0),
	}
	srv := httptest.NewServer(s.Handler(2))
	defer srv.Close()

	get := func(path string) (*http.Response, []byte) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}

	resp, body := get("/lines/16/34501/23013.png")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" ||
		resp.Header.Get("Access-Control-Allow-Origin") != "*" || resp.Header.Get("Cache-Control") != "public, max-age=3600" {
		t.Errorf("tile: %d %v", resp.StatusCode, resp.Header)
	}
	if _, err := png.Decode(bytes.NewReader(body)); err != nil {
		t.Errorf("tile is not a PNG: %v", err)
	}
	// Without .png, like the current URL.
	if resp, _ := get("/lines/16/34501/23013"); resp.StatusCode != 200 {
		t.Errorf("tile without .png: %d", resp.StatusCode)
	}
	// Empty tile: transparent PNG.
	if resp, body := get("/lines/16/0/0.png"); resp.StatusCode != 200 || len(body) == 0 {
		t.Errorf("empty tile: %d", resp.StatusCode)
	}
	// Low zoom: from disk, or empty when missing.
	if _, body := get("/lines/2/2/1.png"); string(body) != "disk tile" {
		t.Errorf("disk tile: %q", body)
	}
	if resp, body := get("/lines/2/0/0.png"); resp.StatusCode != 200 || !bytes.Equal(body, s.empty) {
		t.Errorf("missing disk tile should be empty")
	}
	for _, bad := range []string{"/lines/21/0/0.png", "/lines/2/4/0.png", "/lines/a/0/0", "/lines/2/-1/0"} {
		if resp, _ := get(bad); resp.StatusCode != 404 {
			t.Errorf("%s: %d, want 404", bad, resp.StatusCode)
		}
	}
	if resp, body := get("/"); resp.StatusCode != 200 || !bytes.Contains(body, []byte("lines/{z}/{x}/{y}.png")) {
		t.Errorf("viewer: %d", resp.StatusCode)
	}
	if resp, _ := get("/nothing"); resp.StatusCode != 404 {
		t.Errorf("unknown path: %d, want 404", resp.StatusCode)
	}
	if resp, _ := get("/health"); resp.StatusCode != 200 {
		t.Errorf("health: %d", resp.StatusCode)
	}
}
