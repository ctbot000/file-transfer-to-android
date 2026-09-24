// Package qrcode encodes text as a QR code and renders it as SVG markup or as
// terminal text.
package qrcode

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"rsc.io/qr/coding"
)

// Code is an encoded QR symbol: a square grid of dark and light modules,
// without the surrounding quiet zone.
type Code struct {
	size int
	dark []bool
}

// Encode encodes text at error correction level M.
//
// It evaluates all eight masks and keeps the one with the lowest penalty
// score, as the QR specification recommends; rsc.io/qr's own Encode always
// uses mask 0, which can leave finder-like patterns that slow scanners down.
func Encode(text string) (*Code, error) {
	var enc coding.Encoding
	switch {
	case coding.Num(text).Check() == nil:
		enc = coding.Num(text)
	case coding.Alpha(text).Check() == nil:
		enc = coding.Alpha(text)
	default:
		enc = coding.String(text)
	}

	const level = coding.M
	v := coding.Version(coding.MinVersion)
	for enc.Bits(v) > v.DataBytes(level)*8 {
		if v == coding.MaxVersion {
			return nil, errors.New("qrcode: text too long to encode")
		}
		v++
	}

	var best *Code
	bestScore := 0
	for mask := coding.Mask(0); mask < 8; mask++ {
		plan, err := coding.NewPlan(v, level, mask)
		if err != nil {
			return nil, err
		}
		cc, err := plan.Encode(enc)
		if err != nil {
			return nil, err
		}
		c := &Code{size: cc.Size, dark: make([]bool, cc.Size*cc.Size)}
		for y := 0; y < cc.Size; y++ {
			for x := 0; x < cc.Size; x++ {
				c.dark[y*cc.Size+x] = cc.Black(x, y)
			}
		}
		if score := c.penalty(); best == nil || score < bestScore {
			best, bestScore = c, score
		}
	}
	return best, nil
}

// Size is the number of modules on each side, excluding the quiet zone.
func (c *Code) Size() int { return c.size }

// Dark reports whether the module at (x, y) is dark. Coordinates outside
// the symbol are light, like the quiet zone around it.
func (c *Code) Dark(x, y int) bool {
	return x >= 0 && x < c.size && y >= 0 && y < c.size && c.dark[y*c.size+x]
}

// SVG returns the code as a standalone SVG document with a quiet zone of
// border modules. It is always dark on light, whatever the page theme,
// because that is what scanners look for.
func (c *Code) SVG(border int) string {
	total := c.size + 2*border
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, total, total)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, total, total)
	for y := 0; y < c.size; y++ {
		for x := 0; x < c.size; {
			if !c.Dark(x, y) {
				x++
				continue
			}
			start := x
			for x < c.size && c.Dark(x, y) {
				x++
			}
			fmt.Fprintf(&b, "M%d %dh%dv1h-%dz", start+border, y+border, x-start, x-start)
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}

// 256-color palette entries for pure black and pure white. Unlike the
// 16 basic colors, terminal themes do not remap these.
const (
	black = 16
	white = 231
)

// WriteTerminal draws the code with upper-half-block characters, two
// modules per character cell, prefixing every line with indent. Colors are
// set explicitly so the code reads dark on light in dark and light terminal
// themes alike.
func (c *Code) WriteTerminal(w io.Writer, border int, indent string) error {
	total := c.size + 2*border
	color := func(x, y int) int {
		if y < total && c.Dark(x-border, y-border) {
			return black
		}
		return white
	}
	var b strings.Builder
	for y := 0; y < total; y += 2 {
		b.WriteString(indent)
		fg, bg := -1, -1
		for x := 0; x < total; x++ {
			if top := color(x, y); top != fg {
				fmt.Fprintf(&b, "\x1b[38;5;%dm", top)
				fg = top
			}
			if bottom := color(x, y+1); bottom != bg {
				fmt.Fprintf(&b, "\x1b[48;5;%dm", bottom)
				bg = bottom
			}
			b.WriteString("▀")
		}
		b.WriteString("\x1b[0m\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// penalty scores the symbol with the four rules of ISO/IEC 18004 section
// 7.8.3. Lower is easier to scan.
func (c *Code) penalty() int {
	n := c.size
	row := func(y int) func(int) bool { return func(x int) bool { return c.Dark(x, y) } }
	col := func(x int) func(int) bool { return func(y int) bool { return c.Dark(x, y) } }

	score := 0
	for i := 0; i < n; i++ {
		// N1: runs of five or more modules of one color.
		score += runPenalty(row(i), n) + runPenalty(col(i), n)
		// N3: finder-like 1:1:3:1:1 patterns next to four light modules.
		score += 40 * (finderLike(row(i), n) + finderLike(col(i), n))
	}
	// N2: 2x2 blocks of one color.
	for y := 0; y < n-1; y++ {
		for x := 0; x < n-1; x++ {
			d := c.Dark(x, y)
			if c.Dark(x+1, y) == d && c.Dark(x, y+1) == d && c.Dark(x+1, y+1) == d {
				score += 3
			}
		}
	}
	// N4: distance of the dark-module ratio from one half.
	dark := 0
	for _, d := range c.dark {
		if d {
			dark++
		}
	}
	deviation := dark*100/(n*n) - 50
	if deviation < 0 {
		deviation = -deviation
	}
	return score + 10*(deviation/5)
}

func runPenalty(dark func(int) bool, n int) int {
	score, run := 0, 1
	for i := 1; i <= n; i++ {
		if i < n && dark(i) == dark(i-1) {
			run++
			continue
		}
		if run >= 5 {
			score += 3 + run - 5
		}
		run = 1
	}
	return score
}

func finderLike(dark func(int) bool, n int) int {
	pattern := [7]bool{true, false, true, true, true, false, true}
	at := func(i int) bool { return i >= 0 && i < n && dark(i) }
	count := 0
	for i := 0; i+len(pattern) <= n; i++ {
		match := true
		for k, d := range pattern {
			if at(i+k) != d {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		lightBefore, lightAfter := true, true
		for k := 1; k <= 4; k++ {
			lightBefore = lightBefore && !at(i-k)
			lightAfter = lightAfter && !at(i+len(pattern)-1+k)
		}
		if lightBefore {
			count++
		}
		if lightAfter {
			count++
		}
	}
	return count
}
