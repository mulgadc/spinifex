package consolebranding

// ThemeTokens exposes the built-in tokens the colour check uses, by theme and
// name, as oklch() lightness, chroma and hue.
func ThemeTokens() map[string]map[string][3]float64 {
	out := map[string]map[string][3]float64{}
	for name, tokens := range map[string][]token{"light": lightTokens, "dark": darkTokens} {
		out[name] = map[string][3]float64{}
		for _, t := range tokens {
			out[name][t.name] = [3]float64{t.l, t.c, t.h}
		}
	}
	return out
}
