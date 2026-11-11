package tiles

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"

	"github.com/ericpauley/go-quantize/quantize"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Rub21/gpxtiler/importer"
)

const (
	// TileSize is the size of the PNG tiles in pixels.
	TileSize = 256
	mercMax  = 20037508.342789244
	// One datamaps world unit in meters: datamaps uses 2^32 units per world.
	worldUnit = 2 * mercMax / (1 << 32)
)

// Brightness of each line at zoom z, from datamaps render.c for lines:
// dot_bright * line_per_dot / line_thick * dot_ramp^(z - dot_base).
func lineBright(z int) float64 {
	const (
		dotBright  = 0.05917
		linePerDot = 6.64
		dotRamp    = 1.23
		dotBase    = 13
	)
	return dotBright * linePerDot * math.Pow(dotRamp, float64(z-dotBase))
}

// jumpLength is the length in meters after which a line gets dimmer, like
// the -g option of datamaps: 1600 world units (about 15 m) at zoom 16, x1.5
// for each zoom out. So GPS jumps almost disappear.
func jumpLength(z int) float64 {
	return 1600 * worldUnit * math.Pow(1.5, float64(16-z))
}

// Renderer draws PNG tiles from the trace tables.
type Renderer struct {
	Pool *pgxpool.Pool
	// LowZooms: the zooms with a grid table of the lines (trace_grid_z<N>,
	// made by import). A tile reads the table of the lowest zoom that is
	// not below its own; above the highest table it reads trace_segments.
	// Use LoadLowZooms to fill it from the database.
	LowZooms []int
	// Colors: reduce each PNG to this many colors, like pngquant does for
	// the current tiles (64). The file gets about 4 times smaller. 0 keeps
	// full color.
	Colors int
}

// LoadLowZooms reads from the database which grid tables exist.
func (r *Renderer) LoadLowZooms(ctx context.Context) error {
	conn, err := r.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	r.LowZooms, err = importer.LowZooms(ctx, conn.Conn())
	return err
}

// table returns the table to read for a tile zoom, and whether it is a
// grid table.
func (r *Renderer) table(z int) (string, bool) {
	for _, lz := range r.LowZooms { // sorted
		if z <= lz {
			return importer.LowTable(lz), true
		}
	}
	return "trace_segments", false
}

// tileBounds returns the Web Mercator bounds of a tile.
func tileBounds(z, x, y int) (minx, maxy, size float64) {
	size = 2 * mercMax / float64(int(1)<<z)
	return -mercMax + float64(x)*size, mercMax - float64(y)*size, size
}

// HasLines tells whether any line touches the tile. It only reads the
// index, so it is much cheaper than drawing the tile.
func (r *Renderer) HasLines(ctx context.Context, z, x, y int) (bool, error) {
	table, _ := r.table(z)
	var found bool
	err := r.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+table+" WHERE geom && ST_TileEnvelope($1, $2, $3, margin => 1.0 / 256))", z, x, y).Scan(&found)
	return found, err
}

// Render returns the PNG of one tile, or nil when the tile is empty.
func (r *Renderer) Render(ctx context.Context, z, x, y int) ([]byte, error) {
	c, err := r.Draw(ctx, z, x, y)
	if err != nil || c.Empty() {
		return nil, err
	}
	var img image.Image = c.Image()
	if r.Colors > 0 {
		img = quantizeImage(img, r.Colors)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// quantizeImage reduces the image to n colors (median cut, no dithering),
// keeping the transparent background.
func quantizeImage(img image.Image, n int) *image.Paletted {
	q := quantize.MedianCutQuantizer{AddTransparent: true}
	pal := q.Quantize(make(color.Palette, 0, n), img)
	dst := image.NewPaletted(img.Bounds(), pal)
	draw.Draw(dst, dst.Bounds(), img, image.Point{}, draw.Src)
	return dst
}

// Draw draws one tile on a new canvas.
func (r *Renderer) Draw(ctx context.Context, z, x, y int) (*Canvas, error) {
	// Rows touching the tile, with one pixel of margin. Lines are not
	// clipped or simplified in SQL: that would change the length of the
	// edges and so their brightness. Import already cuts lines every 1000
	// points or 50 km, so the rows are small.
	table, low := r.table(z)
	sql := "SELECT ST_AsBinary(geom), 1 FROM " + table + " WHERE geom && ST_TileEnvelope($1, $2, $3, margin => 1.0 / 256)"
	if low {
		// A grid edge stands for n traces: drawn once with n times the
		// brightness, the same as n lines on top of each other.
		sql = "SELECT ST_AsBinary(geom), n FROM " + table + " WHERE geom && ST_TileEnvelope($1, $2, $3, margin => 1.0 / 256)"
	}
	rows, err := r.Pool.Query(ctx, sql, z, x, y)
	if err != nil {
		return nil, err
	}

	c := NewCanvas(TileSize)
	minx, maxy, size := tileBounds(z, x, y)
	scale := TileSize / size
	bright := lineBright(z)
	jump := jumpLength(z)
	if low {
		// The grid tables are already cut at the GPS jumps, so nothing is
		// dimmed.
		jump = math.Inf(1)
	}

	var wkb []byte
	var n int
	_, err = pgx.ForEachRow(rows, []any{&wkb, &n}, func() error {
		pts, err := importer.ParseWKBLineString(wkb)
		if err != nil {
			return err
		}
		for i := 1; i < len(pts); i++ {
			a, b := pts[i-1], pts[i]
			dx, dy := b[0]-a[0], b[1]-a[1]
			if dx == 0 && dy == 0 {
				continue
			}
			br := bright * float64(n)
			if d := math.Hypot(dx, dy); d > jump {
				br /= d / jump
				if br < .0025 {
					continue
				}
			}
			// Direction of travel as a turn: 0 = east, counterclockwise,
			// the same angle gpx-import gives datamaps (-C 256).
			hue := math.Atan2(dy, dx) / (2 * math.Pi)
			if hue < 0 {
				hue++
			}
			c.Line((a[0]-minx)*scale, (maxy-a[1])*scale, (b[0]-minx)*scale, (maxy-b[1])*scale, br, hue)
		}
		return nil
	})
	return c, err
}
