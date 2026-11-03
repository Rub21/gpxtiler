package importer

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Expire collects the tiles touched by inserted or deleted traces and
// writes them to a file, one "z/x/y" per line, in one zoom.
//
// While import runs the file ends in ".part". At the end it is renamed to
// ".txt", so render only reads finished files. A nil *Expire does nothing.
type Expire struct {
	zoom    int
	tiles   map[uint64]struct{}
	pending []uint64
	path    string // final path, without ".part"
	file    *os.File
}

// NewExpire prepares an expire file in dir. Leftover ".part" files from a
// run that stopped are renamed to ".txt" first, so their tiles are not lost.
func NewExpire(dir string, zoom int, runTime time.Time) (*Expire, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	left, err := filepath.Glob(filepath.Join(dir, "*.txt.part"))
	if err != nil {
		return nil, err
	}
	for _, p := range left {
		if err := os.Rename(p, strings.TrimSuffix(p, ".part")); err != nil {
			return nil, err
		}
	}
	name := "expire-" + runTime.UTC().Format("20060102T150405Z") + ".txt"
	return &Expire{
		zoom:  zoom,
		tiles: map[uint64]struct{}{},
		path:  filepath.Join(dir, name),
	}, nil
}

// AddLine marks every tile the line goes through.
func (e *Expire) AddLine(pts []XY) {
	if e == nil || len(pts) == 0 {
		return
	}
	if len(pts) == 1 {
		e.addSegment(pts[0], pts[0])
	}
	for i := 1; i < len(pts); i++ {
		e.addSegment(pts[i-1], pts[i])
	}
}

// addSegment walks the tile grid from a to b (Amanatides & Woo), so tiles
// that the line only cuts at a corner are also marked.
func (e *Expire) addSegment(a, b XY) {
	n := 1 << e.zoom
	size := 2 * mercMax / float64(n)
	ax, ay := (a[0]+mercMax)/size, (mercMax-a[1])/size
	bx, by := (b[0]+mercMax)/size, (mercMax-b[1])/size
	x, y := int(math.Floor(ax)), int(math.Floor(ay))
	endX, endY := int(math.Floor(bx)), int(math.Floor(by))
	e.addTile(x, y, n)

	dx, dy := bx-ax, by-ay
	stepX, tMaxX, tDeltaX := gridStep(ax, dx, x)
	stepY, tMaxY, tDeltaY := gridStep(ay, dy, y)
	maxSteps := abs(endX-x) + abs(endY-y) + 2
	for i := 0; (x != endX || y != endY) && i < maxSteps; i++ {
		if tMaxX < tMaxY {
			x += stepX
			tMaxX += tDeltaX
		} else {
			y += stepY
			tMaxY += tDeltaY
		}
		e.addTile(x, y, n)
	}
}

func gridStep(start, d float64, cell int) (step int, tMax, tDelta float64) {
	switch {
	case d > 0:
		return 1, (float64(cell+1) - start) / d, 1 / d
	case d < 0:
		return -1, (float64(cell) - start) / d, -1 / d
	}
	return 0, math.Inf(1), math.Inf(1)
}

func (e *Expire) addTile(x, y, n int) {
	x = max(0, min(n-1, x))
	y = max(0, min(n-1, y))
	k := uint64(x)<<32 | uint64(y)
	if _, ok := e.tiles[k]; ok {
		return
	}
	e.tiles[k] = struct{}{}
	e.pending = append(e.pending, k)
}

// Flush writes the new tiles to disk. Import calls it before each commit,
// so a tile is never lost: at worst it is written twice.
func (e *Expire) Flush() error {
	if e == nil || len(e.pending) == 0 {
		return nil
	}
	if e.file == nil {
		f, err := os.OpenFile(e.path+".part", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		e.file = f
	}
	w := bufio.NewWriter(e.file)
	for _, k := range e.pending {
		fmt.Fprintf(w, "%d/%d/%d\n", e.zoom, k>>32, k&0xffffffff)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := e.file.Sync(); err != nil {
		return err
	}
	e.pending = e.pending[:0]
	return nil
}

// Close writes the last tiles and renames the file to ".txt".
// It returns the final path, or "" when there were no tiles.
func (e *Expire) Close() (string, error) {
	if e == nil {
		return "", nil
	}
	if err := e.Flush(); err != nil {
		return "", err
	}
	if e.file == nil {
		return "", nil
	}
	if err := e.file.Close(); err != nil {
		return "", err
	}
	e.file = nil
	return e.path, os.Rename(e.path+".part", e.path)
}

// Count returns the number of tiles marked in this run.
func (e *Expire) Count() int {
	if e == nil {
		return 0
	}
	return len(e.tiles)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
