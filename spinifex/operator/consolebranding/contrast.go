package consolebranding

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
)

// rgb is a gamma-encoded sRGB colour with channels in [0, 1].
type rgb struct{ r, g, b float64 }

var hexColorRE = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// parseHex reads a #RRGGBB colour. The strict shape is what makes the
// generated stylesheet safe: no accepted value can carry CSS syntax.
func parseHex(s string) (rgb, bool) {
	if !hexColorRE.MatchString(s) {
		return rgb{}, false
	}
	n, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return rgb{}, false
	}
	return rgb{float64(n>>16&0xff) / 255, float64(n>>8&0xff) / 255, float64(n&0xff) / 255}, true
}

// oklch converts a CSS oklch() colour (l in [0, 1], hue in degrees) to sRGB,
// clamped to the gamut, as a browser renders it.
func oklch(l, c, hue float64) rgb {
	h := hue * math.Pi / 180
	a, b := c*math.Cos(h), c*math.Sin(h)
	lp := l + 0.3963377774*a + 0.2158037573*b
	mp := l - 0.1055613458*a - 0.0638541728*b
	sp := l - 0.0894841775*a - 1.2914855480*b
	l3, m3, s3 := lp*lp*lp, mp*mp*mp, sp*sp*sp
	return rgb{
		encode(4.0767416621*l3 - 3.3077115913*m3 + 0.2309699292*s3),
		encode(-1.2684380046*l3 + 2.6097574011*m3 - 0.3413193965*s3),
		encode(-0.0041960863*l3 - 0.7034186147*m3 + 1.7076147010*s3),
	}
}

func encode(v float64) float64 {
	v = min(max(v, 0), 1)
	if v <= 0.0031308 {
		return 12.92 * v
	}
	return 1.055*math.Pow(v, 1/2.4) - 0.055
}

func decode(v float64) float64 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

// luminance is the WCAG 2 relative luminance.
func (c rgb) luminance() float64 {
	return 0.2126*decode(c.r) + 0.7152*decode(c.g) + 0.0722*decode(c.b)
}

// over composites c at the given opacity on top of ground, as a browser does.
func (c rgb) over(ground rgb, alpha float64) rgb {
	mix := func(f, g float64) float64 { return alpha*f + (1-alpha)*g }
	return rgb{mix(c.r, ground.r), mix(c.g, ground.g), mix(c.b, ground.b)}
}

func contrast(x, y rgb) float64 {
	lx, ly := x.luminance(), y.luminance()
	if lx < ly {
		lx, ly = ly, lx
	}
	return (lx + 0.05) / (ly + 0.05)
}

// token is one oklch() declaration from the console's styles.css.
type token struct {
	name    string
	l, c, h float64
}

// The tokens a primary colour is drawn against, in each theme.
var (
	lightTokens = []token{
		{"--background", 0.958, 0.007, 88.6},
		{"--card", 1, 0, 0},
		{"--primary-foreground", 1, 0, 0},
	}
	darkTokens = []token{
		{"--background", 0.207, 0.012, 254.1},
		{"--card", 0.242, 0.015, 256.8},
		{"--primary-foreground", 0.211, 0.012, 254.1},
	}
)

type theme struct {
	name                         string
	background, card, primaryInk rgb
}

func newTheme(name string, tokens []token) theme {
	t := theme{name: name}
	for _, tk := range tokens {
		v := oklch(tk.l, tk.c, tk.h)
		switch tk.name {
		case "--background":
			t.background = v
		case "--card":
			t.card = v
		case "--primary-foreground":
			t.primaryInk = v
		}
	}
	return t
}

var (
	lightTheme = newTheme("light", lightTokens)
	darkTheme  = newTheme("dark", darkTokens)
)

// Minimum ratios: text at 4.5:1 and a focus indicator at 3:1.
const (
	minTextContrast = 4.5
	minRingContrast = 3.0
	// hoverOpacity is the button's hover:bg-primary/80 fill.
	hoverOpacity = 0.8
)

// checkPrimary reports the first use of --primary that the colour fails in
// the theme: button text at rest and on hover, link text or the focus ring.
// Only a colour the operator sets is checked; an unset one keeps the built-in.
func checkPrimary(t theme, c rgb) error {
	checks := []struct {
		use  string
		x, y rgb
		min  float64
	}{
		{"button text", t.primaryInk, c, minTextContrast},
		{"button hover text", t.primaryInk, c.over(t.background, hoverOpacity), minTextContrast},
		{"link text on the page", c, t.background, minTextContrast},
		{"link text on a card", c, t.card, minTextContrast},
		{"focus ring", c, t.background, minRingContrast},
	}
	for _, ch := range checks {
		if r := contrast(ch.x, ch.y); r < ch.min {
			return fmt.Errorf("%s contrast is %.2f:1 in the %s theme; at least %.1f:1 is needed", ch.use, r, t.name, ch.min)
		}
	}
	return nil
}
