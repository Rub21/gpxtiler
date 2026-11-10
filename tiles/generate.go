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

// Generator writes the tiles on disk, in advance (All) or when they
// expire (Expire).
type Generator struct {
	Renderer *Renderer
	TileDir  string
	Workers  int
	Log      *log.Logger
}

// Stats is the result of one run.
type Stats struct {
	Drawn   int // tiles drawn from the database
	Written int // tiles with lines, written to disk
	Deleted int // tiles removed from disk (empty, or expired from the cache)
	Files   int // expire files read
}

// All draws and writes every tile from minZoom to maxZoom that has lines.
// It walks the tiles as a quadtree: the children of an empty tile are
// empty too, so they are skipped. Below minZoom the tiles are not drawn,
// only checked for lines.
func (g *Generator) All(ctx context.Context, minZoom, maxZoom int) (Stats, error) {
	var st Stats
	level := []Tile{{0, 0, 0}}
	for z := 0; z <= maxZoom && len(level) > 0; z++ {
		var next []Tile
		var mu sync.Mutex
		err := g.each(ctx, level, func(t Tile) error {
			if t.Z < minZoom {
				has, err := g.Renderer.HasLines(ctx, t.Z, t.X, t.Y)
				if err != nil || !has {
					return err
				}
			} else {
				data, err := g.draw(ctx, &st, t)
				if err != nil || data == nil {
					return err
				}
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
// them: the tiles from minZoom to maxZoom are drawn again, the tiles above
// maxZoom up to cacheZoom are removed from disk, so serve draws them on
// the next request. Each file is deleted after its tiles are done.
func (g *Generator) Expire(ctx context.Context, expireDir string, minZoom, maxZoom, cacheZoom int) (Stats, error) {
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
		if t.Z < minZoom || t.Z > cacheZoom {
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
		if t.Z > maxZoom {
			was, err := DeleteTile(g.TileDir, t.Z, t.X, t.Y)
			if was {
				deleted.Add(1)
			}
			return err
		}
		_, err := g.draw(ctx, &st, t)
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

// draw renders one tile and writes the PNG to disk, or removes the old
// file when the tile is empty now.
func (g *Generator) draw(ctx context.Context, st *Stats, t Tile) ([]byte, error) {
	data, err := g.Renderer.Render(ctx, t.Z, t.X, t.Y)
	if err != nil {
		return nil, fmt.Errorf("tile %s: %w", t, err)
	}
	g.count(st, func() { st.Drawn++ })
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
