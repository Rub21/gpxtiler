package tiles

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// The tiles on disk, DIR/z/x/y.png: the pre-rendered low zooms written by
// render, and the cache of the zooms drawn on demand by serve.

// TilePath is where a tile is stored on disk.
func TilePath(dir string, z, x, y int) string {
	return filepath.Join(dir, strconv.Itoa(z), strconv.Itoa(x), strconv.Itoa(y)+".png")
}

// ReadTile returns the tile from disk, or nil when it is not there.
func ReadTile(dir string, z, x, y int) []byte {
	data, err := os.ReadFile(TilePath(dir, z, x, y))
	if err != nil {
		return nil
	}
	return data
}

// WriteTile stores a tile. It writes a temporary file and renames it, so a
// reader never sees a half-written tile.
func WriteTile(dir string, z, x, y int, data []byte) error {
	path := TilePath(dir, z, x, y)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), strconv.Itoa(y)+".*.part")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// DeleteTile removes a tile from disk. It returns whether the tile was
// there; a missing tile is not an error.
func DeleteTile(dir string, z, x, y int) (bool, error) {
	err := os.Remove(TilePath(dir, z, x, y))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Lock takes a lock on DIR/render.lock, so only one render writes the
// tiles at a time. A second render waits. Close the file to release it.
func Lock(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "render.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
	}
	return f, nil
}
