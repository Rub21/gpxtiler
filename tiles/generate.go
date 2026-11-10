package tiles

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Tile is one z/x/y.
type Tile struct{ Z, X, Y int }

func (t Tile) String() string { return fmt.Sprintf("%d/%d/%d", t.Z, t.X, t.Y) }

// Generator writes the tiles on disk: the pre-rendered low zooms (up to
// PreZoom) and the cache above them (up to CacheZoom).
type Generator struct {
	Renderer  *Renderer
	TileDir   string
	PreZoom   int // tiles up to this zoom are pre-rendered
	CacheZoom int // tiles from PreZoom+1 to this zoom are cached by serve
	Workers   int
	Log       *log.Logger
}

// Stats is the result of one run.
type Stats struct {
	Drawn   int // tiles drawn from the database
	Written int // tiles with lines, written to disk
	Deleted int // tiles removed from disk (empty, or expired from the cache)
	Files   int // expire files read
}

// All draws every tile from zoom 0 to maxZoom that has lines, and writes
// the ones from minZoom up. It walks the tiles as a quadtree: the children
// of an empty tile are empty too, so they are skipped.
func (g *Generator) All(ctx context.Context, minZoom, maxZoom int) (Stats, error) {
	var st Stats
	if maxZoom > g.PreZoom {
		return st, fmt.Errorf("max zoom %d is above the pre-rendered zooms (%d)", maxZoom, g.PreZoom)
	}
	level := []Tile{{0, 0, 0}}
	for z := 0; z <= maxZoom && len(level) > 0; z++ {
		var next []Tile
		var mu sync.Mutex
		err := g.each(ctx, level, func(t Tile) error {
			data, err := g.draw(ctx, &st, t, t.Z >= minZoom)
			if err != nil {
				return err
			}
			if data == nil {
				return nil
			}
			mu.Lock()
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					next = append(next, Tile{t.Z + 1, 2*t.X + dx, 2*t.Y + dy})
				}
			}
			mu.Unlock()
			return nil
		})
		if err != nil {
			return st, err
		}
		g.Log.Printf("zoom %d: %d tiles with lines", z, len(next)/4)
		level = next
	}
	return st, nil
}

// Expire reads the expire files in expireDir (in name order) and applies
// them: tiles up to PreZoom and from minZoom up are drawn again, tiles
// above PreZoom are removed from the cache. Each file is deleted after
// its tiles are done.
func (g *Generator) Expire(ctx context.Context, expireDir string, minZoom int) (Stats, error) {
	var st Stats
	files, err := filepath.Glob(filepath.Join(expireDir, "expire-*.txt"))
	if err != nil {
		return st, err
	}
	sort.Strings(files)
	tiles := map[Tile]struct{}{}
	for _, f := range files {
		if err := readExpire(f, tiles); err != nil {
			return st, err
		}
	}
	st.Files = len(files)
	var list []Tile
	for t := range tiles {
		if t.Z > g.CacheZoom || (t.Z <= g.PreZoom && t.Z < minZoom) {
			continue
		}
		list = append(list, t)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Z != list[j].Z {
			return list[i].Z < list[j].Z
		}
		return list[i].X < list[j].X || (list[i].X == list[j].X && list[i].Y < list[j].Y)
	})
	g.Log.Printf("%d expire files, %d tiles to do", len(files), len(list))

	var deleted atomic.Int64
	err = g.each(ctx, list, func(t Tile) error {
		if t.Z > g.PreZoom {
			was, err := DeleteTile(g.TileDir, t.Z, t.X, t.Y)
			if was {
				deleted.Add(1)
			}
			return err
		}
		_, err := g.draw(ctx, &st, t, true)
		return err
	})
	st.Deleted += int(deleted.Load())
	if err != nil {
		return st, err
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			return st, err
		}
	}
	return st, nil
}

// draw renders one tile. With write, the PNG goes to disk, or the old
// file is removed when the tile is empty now.
func (g *Generator) draw(ctx context.Context, st *Stats, t Tile, write bool) ([]byte, error) {
	data, err := g.Renderer.Render(ctx, t.Z, t.X, t.Y)
	if err != nil {
		return nil, fmt.Errorf("tile %s: %w", t, err)
	}
	g.count(st, func() { st.Drawn++ })
	if !write {
		return data, nil
	}
	if data == nil {
		was, err := DeleteTile(g.TileDir, t.Z, t.X, t.Y)
		if was {
			g.count(st, func() { st.Deleted++ })
		}
		return nil, err
	}
	if err := WriteTile(g.TileDir, t.Z, t.X, t.Y, data); err != nil {
		return nil, err
	}
	g.count(st, func() { st.Written++ })
	return data, nil
}

var statsMu sync.Mutex

func (g *Generator) count(st *Stats, fn func()) {
	statsMu.Lock()
	fn()
	statsMu.Unlock()
}

// each runs fn on the tiles with Workers goroutines. It stops at the first
// error or when ctx is done.
func (g *Generator) each(ctx context.Context, tiles []Tile, fn func(Tile) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan Tile)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for range max(1, g.Workers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				if err := fn(t); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					errMu.Unlock()
				}
			}
		}()
	}
loop:
	for _, t := range tiles {
		select {
		case jobs <- t:
		case <-ctx.Done():
			break loop
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// readExpire adds the z/x/y lines of one expire file to tiles.
func readExpire(path string, tiles map[Tile]struct{}) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var t Tile
		if _, err := fmt.Sscanf(line, "%d/%d/%d", &t.Z, &t.X, &t.Y); err != nil {
			return fmt.Errorf("%s: bad line %q", path, line)
		}
		tiles[t] = struct{}{}
	}
	return sc.Err()
}
