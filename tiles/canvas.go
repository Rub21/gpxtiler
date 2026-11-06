// Package tiles draws the GPS traces as PNG tiles, like the current
// gps-tile.openstreetmap.org tiles made with datamaps.
package tiles

// The drawing and the colors are a port of datamaps by Eric Fischer
// (https://github.com/ericfischer/datamaps, graphics.c), under this license:
//
// Copyright (c) 2013-2014, Mapbox Inc.
// All rights reserved.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions
// are met:
//
// - Redistributions of source code must retain the above copyright notice,
//   this list of conditions and the following disclaimer.
// - Redistributions in binary form must reproduce the above copyright notice,
//   this list of conditions and the following disclaimer in the documentation
//   and/or other materials provided with the distribution.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
// HOLDER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
// LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
// DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
// THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

import (
	"image"
	"math"
)

// Canvas adds up the brightness and the direction of every line that goes
// through each pixel. Encode turns that into colors.
type Canvas struct {
	Size   int
	bright []float64 // sum of brightness
	cx, cy []float64 // sum of brightness * direction, as a vector
}

// NewCanvas returns an empty canvas of size x size pixels.
func NewCanvas(size int) *Canvas {
	n := size * size
	return &Canvas{Size: size, bright: make([]float64, n), cx: make([]float64, n), cy: make([]float64, n)}
}

// Empty is true when nothing was drawn.
func (c *Canvas) Empty() bool {
	for _, b := range c.bright {
		if b != 0 {
			return false
		}
	}
	return true
}

// put adds brightness to one pixel. hue is the direction as a turn (0-1).
func (c *Canvas) put(x, y, bright, hue float64) {
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	if x0 < 0 || y0 < 0 || x0 >= c.Size || y0 >= c.Size {
		return
	}
	i := y0*c.Size + x0
	c.bright[i] += bright
	s, co := math.Sincos(hue * 2 * math.Pi)
	c.cx[i] += bright * co
	c.cy[i] += bright * s
}

func fpart(x float64) float64  { return x - math.Floor(x) }
func rfpart(x float64) float64 { return 1 - fpart(x) }

// Line draws an antialiased line, from datamaps antialiasedLine (based on
// Xiaolin Wu's line algorithm). It clips the line to the canvas first.
func (c *Canvas) Line(x0, y0, x1, y1, bright, hue float64) {
	m := float64(c.Size) + 1
	var ok bool
	if x0, y0, x1, y1, ok = clip(x0, y0, x1, y1, -2, -2, m, m); !ok {
		return
	}

	steep := math.Abs(y1-y0) > math.Abs(x1-x0)
	if steep {
		x0, y0 = y0, x0
		x1, y1 = y1, x1
	}
	if x0 > x1 {
		x0, x1 = x1, x0
		y0, y1 = y1, y0
	}
	plot := func(x, y, b float64) {
		if steep {
			c.put(y, x, b, hue)
		} else {
			c.put(x, y, b, hue)
		}
	}

	dx := x1 - x0
	dy := y1 - y0
	gradient := dy / dx

	// Start and end of the line are in the same pixel.
	if math.Floor(x0) == math.Floor(x1) {
		y0 = (y0 + y1) / 2
		plot(x0, y0, dx*rfpart(y0)*bright)
		plot(x0, y0+1, dx*fpart(y0)*bright)
		return
	}
	// Fractional pixel at the start.
	if x0 != math.Floor(x0) {
		yy := y0 + .5*rfpart(x0)*gradient
		plot(x0, yy, rfpart(x0)*rfpart(yy)*bright)
		plot(x0, yy+1, rfpart(x0)*fpart(yy)*bright)
		y0 += gradient * rfpart(x0)
		x0 = math.Ceil(x0)
	}
	// Fractional pixel at the end.
	if x1 != math.Floor(x1) {
		yy := y1 - .5*fpart(x1)*gradient
		plot(x1, yy, fpart(x1)*rfpart(yy)*bright)
		plot(x1, yy+1, fpart(x1)*fpart(yy)*bright)
		y1 -= gradient * fpart(x1)
		x1 = math.Floor(x1)
	}
	// Whole pixels. The middle of each pixel is halfway through a step.
	y0 += .5 * gradient
	for ; x0 < x1; x0++ {
		plot(x0, y0, rfpart(y0)*bright)
		plot(x0, y0+1, fpart(y0)*bright)
		y0 += gradient
	}
}

// clip cuts the line to the box (Liang-Barsky). ok is false when the line
// is outside.
func clip(x0, y0, x1, y1, xmin, ymin, xmax, ymax float64) (float64, float64, float64, float64, bool) {
	t0, t1 := 0.0, 1.0
	dx, dy := x1-x0, y1-y0
	for _, e := range [4][2]float64{{-dx, x0 - xmin}, {dx, xmax - x0}, {-dy, y0 - ymin}, {dy, ymax - y0}} {
		p, q := e[0], e[1]
		if p == 0 {
			if q < 0 {
				return 0, 0, 0, 0, false
			}
			continue
		}
		r := q / p
		if p < 0 {
			if r > t1 {
				return 0, 0, 0, 0, false
			}
			t0 = max(t0, r)
		} else {
			if r < t0 {
				return 0, 0, 0, 0, false
			}
			t1 = min(t1, r)
		}
	}
	return x0 + t0*dx, y0 + t0*dy, x0 + t1*dx, y0 + t1*dy, true
}

// Image turns the canvas into colors, like datamaps out() with the options
// of gps-tile: transparent background (-t 0), gamma 0.5, color cap 0.7.
//   - up to half brightness: from transparent to the color of the direction
//   - from half to full brightness: from the color to white
//   - over full brightness: white
//
// When lines in many directions cross, the color goes to gray.
func (c *Canvas) Image() *image.NRGBA {
	const (
		gamma    = 0.5
		limit2   = 1.0
		limit    = limit2 / 2
		colorCap = 0.7
		gray     = 128.0
	)
	img := image.NewNRGBA(image.Rect(0, 0, c.Size, c.Size))
	for i, v := range c.bright {
		if v == 0 {
			continue // transparent
		}
		midr, midg, midb := gray, gray, gray
		sat := 0.0
		if c.cx[i] != 0 || c.cy[i] != 0 {
			h := math.Atan2(c.cy[i], c.cx[i]) / (2 * math.Pi)
			sat = math.Hypot(c.cx[i], c.cy[i]) / v
			// http://basecase.org/env/on-rainbows
			h += .5
			h *= -1
			r1 := math.Sin(math.Pi * h)
			g1 := math.Sin(math.Pi * (h + 1.0/3))
			b1 := math.Sin(math.Pi * (h + 2.0/3))
			midr = 255*r1*r1*sat + gray*(1-sat)
			midg = 255*g1*g1*sat + gray*(1-sat)
			midb = 255*b1*b1*sat + gray*(1-sat)
		}
		v = math.Exp(math.Log(v) * gamma)
		if sat != 0 && v > limit2*colorCap {
			v = limit2 * colorCap
		}
		p := img.Pix[4*i : 4*i+4]
		switch {
		case v <= limit:
			// From transparent to the color. With a transparent background
			// the color stays and only the opacity changes.
			along := v / limit
			p[0], p[1], p[2], p[3] = byte(midr), byte(midg), byte(midb), byte(along*255)
		case v <= limit2:
			along := (v - limit) / (limit2 - limit)
			p[0] = byte(255*along + midr*(1-along))
			p[1] = byte(255*along + midg*(1-along))
			p[2] = byte(255*along + midb*(1-along))
			p[3] = 255
		default:
			p[0], p[1], p[2], p[3] = 255, 255, 255, 255
		}
	}
	return img
}
