package tiles

import (
	"bytes"
	"context"
	"image/png"
	"math"

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
	// LowZoom: zooms up to this one read trace_segments_low, which is
	// simplified to one pixel at that zoom. The same value as import -low-zoom.
	LowZoom int
}

// tileBounds returns the Web Mercator bounds of a tile.
func tileBounds(z, x, y int) (minx, maxy, size float64) {
	size = 2 * mercMax / float64(int(1)<<z)
	return -mercMax + float64(x)*size, mercMax - float64(y)*size, size
}

// Render returns the PNG of one tile, or nil when the tile is empty.
func (r *Renderer) Render(ctx context.Context, z, x, y int) ([]byte, error) {
	c, err := r.Draw(ctx, z, x, y)
	if err != nil || c.Empty() {
		return nil, err
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, c.Image()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Draw draws one tile on a new canvas.
func (r *Renderer) Draw(ctx context.Context, z, x, y int) (*Canvas, error) {
	// Rows touching the tile, with one pixel of margin. Lines are not
	// clipped or simplified in SQL: that would change the length of the
	// edges and so their brightness. Import already cuts lines every 1000
	// points or 50 km, so the rows are small.
	sql := `SELECT ST_AsBinary(geom) FROM trace_segments
	         WHERE geom && ST_TileEnvelope($1, $2, $3, margin => 1.0 / 256)`
	low := z <= r.LowZoom
	if low {
		sql = `SELECT ST_AsBinary((ST_Dump(geom)).geom) FROM trace_segments_low
		        WHERE geom && ST_TileEnvelope($1, $2, $3, margin => 1.0 / 256)`
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
		// Simplified lines have long edges that are not jumps. Do not dim
		// edges shorter than 3 pixels of the low zoom.
		jump = max(jump, 3*2*mercMax/(TileSize*float64(int(1)<<r.LowZoom)))
	}

	var wkb []byte
	_, err = pgx.ForEachRow(rows, []any{&wkb}, func() error {
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
			br := bright
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
