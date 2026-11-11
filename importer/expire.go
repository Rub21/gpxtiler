package importer

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Expire collects the tiles touched by inserted or deleted traces and
// writes them to a file, one "z/x/y" per line, for every zoom from minZoom
// to maxZoom. It is the format of the osm2pgsql expire files, which
// "tilekiln generate tiles" reads from stdin.
//
// While import runs the file ends in ".part". At the end it is renamed to
// ".txt", so render only reads finished files. A nil *Expire does nothing.
type Expire struct {
	minZoom int
	maxZoom int
	tiles   map[uint64]struct{} // key: zoom<<58 | x<<29 | y
	pending []uint64
	path    string   // final path, without ".part"
	copies  []string // the same file in the other folders
	file    *os.File
}

// NewExpire prepares an expire file in dirs: one folder, or several
// separated by commas, that get the same file (one per reader: the PNG
// render and Tilekiln each delete the files they have applied). Leftover
// ".part" files from a run that stopped are renamed to ".txt" first, so
// their tiles are not lost.
func NewExpire(dirs string, minZoom, maxZoom int, runTime time.Time) (*Expire, error) {
	if minZoom < 0 || maxZoom < minZoom || maxZoom > 28 {
		return nil, fmt.Errorf("expire zooms %d-%d are out of range", minZoom, maxZoom)
	}
	name := "expire-" + runTime.UTC().Format("20060102T150405Z") + ".txt"
	e := &Expire{minZoom: minZoom, maxZoom: maxZoom, tiles: map[uint64]struct{}{}}
	for _, dir := range strings.Split(dirs, ",") {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
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
		if e.path == "" {
			e.path = filepath.Join(dir, name)
		} else {
			e.copies = append(e.copies, filepath.Join(dir, name))
		}
	}
	if e.path == "" {
		return nil, errors.New("no expire folder")
	}
	return e, nil
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
	n := 1 << e.maxZoom
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

// addTile marks a tile at maxZoom and the tiles above it down to minZoom.
func (e *Expire) addTile(x, y, n int) {
	x = max(0, min(n-1, x))
	y = max(0, min(n-1, y))
	for z := e.maxZoom; z >= e.minZoom; z-- {
		k := uint64(z)<<58 | uint64(x)<<29 | uint64(y)
		if _, ok := e.tiles[k]; ok {
			return // the tiles above are marked too
		}
		e.tiles[k] = struct{}{}
		e.pending = append(e.pending, k)
		x, y = x>>1, y>>1
	}
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
		fmt.Fprintf(w, "%d/%d/%d\n", k>>58, (k>>29)&(1<<29-1), k&(1<<29-1))
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
	for _, c := range e.copies {
		if err := copyFile(e.path+".part", c+".part"); err != nil {
			return "", err
		}
		if err := os.Rename(c+".part", c); err != nil {
			return "", err
		}
	}
	return e.path, os.Rename(e.path+".part", e.path)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Count returns the number of tiles marked in this run, in all zooms.
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
