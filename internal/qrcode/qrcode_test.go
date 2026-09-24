package qrcode

import (
	"encoding/xml"
	"fmt"
	"strings"
	"testing"

	"rsc.io/qr/coding"
)

const link = "http://192.168.1.23:8686/k7x2m9qfp3ab/"

// finder reports whether a 7x7 finder pattern has its corner at (x, y).
func finder(c *Code, x, y int) bool {
	for dy := 0; dy < 7; dy++ {
		for dx := 0; dx < 7; dx++ {
			ring := max(abs(dx-3), abs(dy-3))
			if want := ring != 2; c.Dark(x+dx, y+dy) != want {
				return false
			}
		}
	}
	return true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func TestEncodeLaysOutAValidSymbol(t *testing.T) {
	c, err := Encode(link)
	if err != nil {
		t.Fatal(err)
	}
	// 39 bytes at level M need version 3: 29 modules a side.
	if c.Size() != 29 {
		t.Fatalf("size %d, want 29", c.Size())
	}
	n := c.Size()
	if !finder(c, 0, 0) || !finder(c, n-7, 0) || !finder(c, 0, n-7) {
		t.Error("finder patterns missing")
	}
	if finder(c, n-7, n-7) {
		t.Error("a fourth finder pattern")
	}
	for i := 8; i < n-8; i++ { // timing patterns alternate
		if c.Dark(i, 6) != (i%2 == 0) || c.Dark(6, i) != (i%2 == 0) {
			t.Fatalf("timing pattern broken at %d", i)
		}
	}
	if c.Dark(-1, 0) || c.Dark(0, n) {
		t.Error("modules outside the symbol must be light")
	}
}

func TestEncodePicksTheLowestPenaltyMask(t *testing.T) {
	c, err := Encode(link)
	if err != nil {
		t.Fatal(err)
	}
	chosen := c.penalty()
	for mask := coding.Mask(0); mask < 8; mask++ {
		plan, err := coding.NewPlan(3, coding.M, mask)
		if err != nil {
			t.Fatal(err)
		}
		cc, err := plan.Encode(coding.String(link))
		if err != nil {
			t.Fatal(err)
		}
		other := &Code{size: cc.Size, dark: make([]bool, cc.Size*cc.Size)}
		for y := 0; y < cc.Size; y++ {
			for x := 0; x < cc.Size; x++ {
				other.dark[y*cc.Size+x] = cc.Black(x, y)
			}
		}
		if p := other.penalty(); p < chosen {
			t.Errorf("mask %d scores %d, below the chosen %d", mask, p, chosen)
		}
	}
}

func TestPenaltyRules(t *testing.T) {
	// A 5x5 all-dark grid: every row and column is one run of 5 (N1: 3 each),
	// 16 2x2 blocks (N2: 3 each), 100% dark (N4: 10 * 10).
	c := &Code{size: 5, dark: make([]bool, 25)}
	for i := range c.dark {
		c.dark[i] = true
	}
	if got, want := c.penalty(), 10*3+16*3+100; got != want {
		t.Errorf("penalty = %d, want %d", got, want)
	}
	// dark, light, dark x3, light, dark with four light modules after it.
	row := []bool{true, false, true, true, true, false, true, false, false, false, false}
	if got := finderLike(func(i int) bool { return row[i] }, len(row)); got != 2 {
		t.Errorf("finderLike = %d, want 2 (light before, since outside is light, and after)", got)
	}
}

func TestSVG(t *testing.T) {
	c, err := Encode(link)
	if err != nil {
		t.Fatal(err)
	}
	svg := c.SVG(4)
	var doc struct {
		ViewBox string `xml:"viewBox,attr"`
		Path    struct {
			D string `xml:"d,attr"`
		} `xml:"path"`
	}
	if err := xml.Unmarshal([]byte(svg), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.ViewBox != "0 0 37 37" {
		t.Errorf("viewBox = %q", doc.ViewBox)
	}
	// One subpath per horizontal run of dark modules.
	runs := 0
	for y := 0; y < c.Size(); y++ {
		for x := 0; x < c.Size(); x++ {
			if c.Dark(x, y) && !c.Dark(x-1, y) {
				runs++
			}
		}
	}
	if got := strings.Count(doc.Path.D, "M"); got != runs {
		t.Errorf("%d subpaths for %d runs", got, runs)
	}
}

func TestWriteTerminal(t *testing.T) {
	c, err := Encode(link)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := c.WriteTerminal(&b, 2, "  "); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if want := (29 + 4 + 1) / 2; len(lines) != want {
		t.Fatalf("%d lines, want %d", len(lines), want)
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, "  \x1b[") || !strings.HasSuffix(line, "\x1b[0m") {
			t.Fatalf("line %d is not an indented, reset-terminated color line: %q", i, line)
		}
		if n := strings.Count(line, "▀"); n != 33 {
			t.Fatalf("line %d has %d cells, want 33", i, n)
		}
	}
}

// TestWriteTerminalDrawsTheModules reads the colors back out of the
// terminal rendering: each "▀" cell shows the upper module in its
// foreground color and the lower one in its background color.
func TestWriteTerminalDrawsTheModules(t *testing.T) {
	c, err := Encode(link)
	if err != nil {
		t.Fatal(err)
	}
	const border = 2
	var b strings.Builder
	if err := c.WriteTerminal(&b, border, ""); err != nil {
		t.Fatal(err)
	}
	total := c.Size() + 2*border
	for row, line := range strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n") {
		fg, bg, x := -1, -1, 0
		for rest := line; rest != ""; {
			switch {
			case strings.HasPrefix(rest, "\x1b[38;5;"):
				fmt.Sscanf(rest, "\x1b[38;5;%dm", &fg)
				rest = rest[strings.IndexByte(rest, 'm')+1:]
			case strings.HasPrefix(rest, "\x1b[48;5;"):
				fmt.Sscanf(rest, "\x1b[48;5;%dm", &bg)
				rest = rest[strings.IndexByte(rest, 'm')+1:]
			case strings.HasPrefix(rest, "\x1b[0m"):
				rest = rest[len("\x1b[0m"):]
			case strings.HasPrefix(rest, "▀"):
				for dy, color := range []int{fg, bg} {
					y := 2*row + dy
					want := white
					if y < total && c.Dark(x-border, y-border) {
						want = black
					}
					if color != want {
						t.Fatalf("cell (%d, %d): color %d, want %d", x, y, color, want)
					}
				}
				x++
				rest = rest[len("▀"):]
			default:
				t.Fatalf("unexpected output %q", rest[:min(len(rest), 10)])
			}
		}
		if x != total {
			t.Fatalf("row %d has %d cells, want %d", row, x, total)
		}
	}
}
