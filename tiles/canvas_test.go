package tiles

import (
	"math"
	"testing"
)

// The colors of one line in each direction, from the datamaps formula:
// east is red, north is green, west is cyan, south is blue-purple.
func TestDirectionColors(t *testing.T) {
	cases := []struct {
		name         string
		hue          float64
		wantR, wantG byte
		wantB        byte
	}{
		{"east", 0, 255, 63, 63},
		{"north", 0.25, 127, 237, 17},
		{"west", 0.5, 0, 191, 191},
		{"south", 0.75, 127, 17, 237},
	}
	for _, c := range cases {
		cv := NewCanvas(4)
		cv.put(1, 1, 0.1, c.hue)
		p := cv.Image().NRGBAAt(1, 1)
		if absDiff(p.R, c.wantR) > 1 || absDiff(p.G, c.wantG) > 1 || absDiff(p.B, c.wantB) > 1 {
			t.Errorf("%s: got %v, want %d,%d,%d", c.name, p, c.wantR, c.wantG, c.wantB)
		}
		if p.A == 0 || p.A == 255 {
			t.Errorf("%s: alpha %d, want half transparent", c.name, p.A)
		}
	}
}

func absDiff(a, b byte) int {
	return int(math.Abs(float64(a) - float64(b)))
}

// Two lines in opposite directions cancel the color: the pixel is gray.
func TestOppositeDirectionsAreGray(t *testing.T) {
	cv := NewCanvas(4)
	cv.put(1, 1, 0.1, 0)
	cv.put(1, 1, 0.1, 0.5)
	p := cv.Image().NRGBAAt(1, 1)
	if absDiff(p.R, p.G) > 1 || absDiff(p.G, p.B) > 1 {
		t.Errorf("got %v, want gray", p)
	}
}

// Many lines on the same pixel get brighter, but a pixel with a direction
// is capped at 0.7, like datamaps (color cap): 40 % of the way to white.
func TestManyLinesAreCapped(t *testing.T) {
	cv := NewCanvas(4)
	for range 100 {
		cv.put(1, 1, 0.1, 0)
	}
	p := cv.Image().NRGBAAt(1, 1)
	want := [3]float64{255*.4 + 255*.6, 255*.4 + 63.75*.6, 255*.4 + 63.75*.6}
	if absDiff(p.R, byte(want[0])) > 1 || absDiff(p.G, byte(want[1])) > 1 || p.A != 255 {
		t.Errorf("got %v, want %v", p, want)
	}
}

func TestLine(t *testing.T) {
	cv := NewCanvas(16)
	cv.Line(0.5, 8, 15.5, 8, 1, 0)
	for x := 1; x < 15; x++ {
		if cv.bright[8*16+x]+cv.bright[7*16+x] < 0.99 {
			t.Fatalf("pixel %d not drawn: %v", x, cv.bright[8*16+x])
		}
	}
	// A line far outside the canvas draws nothing and returns fast.
	cv = NewCanvas(16)
	cv.Line(-1e9, -1e9, -1e9+5, -1e9, 1, 0)
	cv.Line(-1e9, 8, 1e9, 8, 1, 0) // long line through the canvas is clipped
	if cv.bright[8*16+3] == 0 {
		t.Error("clipped line not drawn")
	}
	if !NewCanvas(4).Empty() {
		t.Error("new canvas not empty")
	}
}

func TestJumpLength(t *testing.T) {
	if d := jumpLength(16); math.Abs(d-14.93) > 0.01 {
		t.Errorf("jump at z16 = %.2f m, want 14.93", d)
	}
	if d := jumpLength(15); math.Abs(d-14.93*1.5) > 0.02 {
		t.Errorf("jump at z15 = %.2f m", d)
	}
}
