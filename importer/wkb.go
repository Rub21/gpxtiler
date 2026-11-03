package importer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const (
	wkbLineString      = 2
	wkbMultiLineString = 5
	ewkbSRIDFlag       = 0x20000000
	srid3857           = 3857
)

// EWKBLineString returns a LineString in EWKB with SRID 3857.
func EWKBLineString(pts []XY) []byte {
	b := make([]byte, 0, 13+16*len(pts))
	b = append(b, 1) // little endian
	b = binary.LittleEndian.AppendUint32(b, wkbLineString|ewkbSRIDFlag)
	b = binary.LittleEndian.AppendUint32(b, srid3857)
	return appendPoints(b, pts)
}

// EWKBMultiLineString returns a MultiLineString in EWKB with SRID 3857.
func EWKBMultiLineString(parts [][]XY) []byte {
	b := []byte{1}
	b = binary.LittleEndian.AppendUint32(b, wkbMultiLineString|ewkbSRIDFlag)
	b = binary.LittleEndian.AppendUint32(b, srid3857)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(parts)))
	for _, p := range parts {
		b = append(b, 1)
		b = binary.LittleEndian.AppendUint32(b, wkbLineString)
		b = appendPoints(b, p)
	}
	return b
}

func appendPoints(b []byte, pts []XY) []byte {
	b = binary.LittleEndian.AppendUint32(b, uint32(len(pts)))
	for _, p := range pts {
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(p[0]))
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(p[1]))
	}
	return b
}

// ParseWKBLineString reads a 2D LineString in WKB or EWKB.
func ParseWKBLineString(b []byte) ([]XY, error) {
	if len(b) < 9 {
		return nil, errors.New("wkb too short")
	}
	var bo binary.ByteOrder = binary.LittleEndian
	if b[0] == 0 {
		bo = binary.BigEndian
	}
	typ := bo.Uint32(b[1:5])
	off := 5
	if typ&ewkbSRIDFlag != 0 {
		off += 4
	}
	if typ&^ewkbSRIDFlag != wkbLineString {
		return nil, fmt.Errorf("wkb type %#x is not a 2D LineString", typ)
	}
	if len(b) < off+4 {
		return nil, errors.New("wkb too short")
	}
	n := int(bo.Uint32(b[off:]))
	off += 4
	if len(b) < off+16*n {
		return nil, errors.New("wkb too short")
	}
	pts := make([]XY, n)
	for i := range pts {
		pts[i][0] = math.Float64frombits(bo.Uint64(b[off:]))
		pts[i][1] = math.Float64frombits(bo.Uint64(b[off+8:]))
		off += 16
	}
	return pts, nil
}
