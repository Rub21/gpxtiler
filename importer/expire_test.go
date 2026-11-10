package importer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExpireTiles(t *testing.T) {
	e := &Expire{minZoom: 1, maxZoom: 1, tiles: map[uint64]struct{}{}}
	// One point in the top left quarter.
	e.AddLine([]XY{ToMercator(-90, 45)})
	if e.Count() != 1 {
		t.Errorf("count = %d", e.Count())
	}
	// From top left to bottom right goes through 3 tiles at zoom 1.
	e = &Expire{minZoom: 1, maxZoom: 1, tiles: map[uint64]struct{}{}}
	e.AddLine([]XY{ToMercator(-90, 10), ToMercator(90, -45)})
	if e.Count() != 3 {
		t.Errorf("count = %d", e.Count())
	}
	// With zooms 0-1 the same line also marks the one tile of zoom 0.
	e = &Expire{minZoom: 0, maxZoom: 1, tiles: map[uint64]struct{}{}}
	e.AddLine([]XY{ToMercator(-90, 10), ToMercator(90, -45)})
	if e.Count() != 4 {
		t.Errorf("count = %d", e.Count())
	}
}

func TestExpireFile(t *testing.T) {
	dir := t.TempDir()
	// A leftover file from a run that stopped.
	old := filepath.Join(dir, "expire-old.txt.part")
	os.WriteFile(old, []byte("14/1/1\n"), 0o644)

	e, err := NewExpire(dir, 12, 14, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "expire-old.txt")); err != nil {
		t.Errorf("leftover file not finished: %v", err)
	}
	e.AddLine([]XY{ToMercator(9.52, 47.14)})
	path, err := e.Close()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "expire-20260102T030405Z.txt" {
		t.Errorf("path = %s", path)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "14/8625/5753\n13/4312/2876\n12/2156/1438\n" {
		t.Errorf("content = %q", b)
	}
	if _, err := NewExpire(dir, 15, 14, time.Now()); err == nil {
		t.Error("min zoom over max zoom should fail")
	}
}
