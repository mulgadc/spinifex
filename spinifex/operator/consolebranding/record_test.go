package consolebranding_test

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	"github.com/mulgadc/spinifex/spinifex/operator/consolebranding"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	pngBytes  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	webpBytes = []byte("RIFF\x10\x00\x00\x00WEBPVP8 ")
	svgBytes  = []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"><rect width="1" height="1"/></svg>`)
)

func imageJSON(t *testing.T, contentType string, data []byte) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(operatorv1.BrandingImage{ContentType: contentType, Data: data})
	require.NoError(t, err)
	return raw
}

func ptr(s string) *string { return &s }

func requireInvalid(t *testing.T, err error, field, reasonPart string) {
	t.Helper()
	var ve *consolebranding.ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, field, ve.Field)
	assert.Contains(t, ve.Reason, reasonPart)
}

func TestApply_ProductName(t *testing.T) {
	_, err := consolebranding.Apply(nil, operatorv1.PutConsoleBrandingRequest{ProductName: strings.Repeat("é", 40)})
	require.NoError(t, err, "40 characters is the limit, counted as characters rather than bytes")

	_, err = consolebranding.Apply(nil, operatorv1.PutConsoleBrandingRequest{ProductName: strings.Repeat("a", 41)})
	requireInvalid(t, err, "productName", "at most 40")

	_, err = consolebranding.Apply(nil, operatorv1.PutConsoleBrandingRequest{ProductName: "Acme\nCloud"})
	requireInvalid(t, err, "productName", "control characters")
}

func TestApply_DocsURL(t *testing.T) {
	rec, err := consolebranding.Apply(nil, operatorv1.PutConsoleBrandingRequest{})
	require.NoError(t, err)
	assert.Nil(t, rec.DocsURL, "absent keeps the built-in link")

	rec, err = consolebranding.Apply(nil, operatorv1.PutConsoleBrandingRequest{DocsURL: ptr("")})
	require.NoError(t, err)
	require.NotNil(t, rec.DocsURL)
	assert.Empty(t, *rec.DocsURL, "empty hides the link")

	_, err = consolebranding.Apply(nil, operatorv1.PutConsoleBrandingRequest{DocsURL: ptr("https://docs.acme.example/start")})
	require.NoError(t, err)

	for _, bad := range []string{
		"http://docs.acme.example",
		"javascript:alert(1)",
		"https://user:pw@docs.acme.example",
		"https:///no-host",
		"https://docs.acme.example/" + strings.Repeat("a", 2048),
	} {
		_, err := consolebranding.Apply(nil, operatorv1.PutConsoleBrandingRequest{DocsURL: ptr(bad)})
		require.Error(t, err, bad)
		requireInvalid(t, err, "docsUrl", "")
	}
}

func TestApply_PrimaryColour(t *testing.T) {
	tests := []struct {
		name    string
		primary operatorv1.BrandingColors
		field   string
		use     string
	}{
		{"not hex", operatorv1.BrandingColors{Light: "blue"}, "primary.light", "#RRGGBB"},
		{"css injection", operatorv1.BrandingColors{Dark: "#fff;}*{x:y"}, "primary.dark", "#RRGGBB"},
		{"short hex", operatorv1.BrandingColors{Light: "#fff"}, "primary.light", "#RRGGBB"},
		{"light button text", operatorv1.BrandingColors{Light: "#0073EE"}, "primary.light", "button text contrast"},
		{"light passes at rest, fails on hover", operatorv1.BrandingColors{Light: "#0050EE"}, "primary.light", "button hover text"},
		{"light passes hover on the page, fails it on a card", operatorv1.BrandingColors{Light: "#585858"}, "primary.light", "button hover text on a card"},
		{"dark button text", operatorv1.BrandingColors{Dark: "#000000"}, "primary.dark", "button text contrast"},
		{"dark passes at rest, fails on hover", operatorv1.BrandingColors{Dark: "#0082EE"}, "primary.dark", "button hover text"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := consolebranding.Apply(nil, operatorv1.PutConsoleBrandingRequest{Primary: tc.primary})
			requireInvalid(t, err, tc.field, tc.use)
		})
	}

	rec, err := consolebranding.Apply(nil, operatorv1.PutConsoleBrandingRequest{Primary: operatorv1.BrandingColors{Light: "#0A4A8F"}})
	require.NoError(t, err)
	assert.Empty(t, rec.Primary.Dark, "a light-only colour leaves the dark theme at its built-in colour")
}

// The built-in colours are never checked: leaving a theme unset, or resetting
// it, keeps the built-in one whatever it scores.
func TestApply_UnsetColourKeepsBuiltIn(t *testing.T) {
	stored := &consolebranding.Record{Primary: operatorv1.BrandingColors{Light: "#0A4A8F", Dark: "#7FB8F0"}}

	rec, err := consolebranding.Apply(stored, operatorv1.PutConsoleBrandingRequest{})
	require.NoError(t, err)
	assert.Equal(t, operatorv1.BrandingColors{}, rec.Primary)
	assert.Empty(t, consolebranding.Stylesheet(rec.Primary))
}

func TestApply_ImagesAreTriState(t *testing.T) {
	stored := &consolebranding.Record{
		Logo:    &operatorv1.BrandingImage{ContentType: consolebranding.TypePNG, Data: pngBytes},
		Favicon: &operatorv1.BrandingImage{ContentType: consolebranding.TypePNG, Data: pngBytes},
	}
	rec, err := consolebranding.Apply(stored, operatorv1.PutConsoleBrandingRequest{
		Favicon:  json.RawMessage("null"),
		LogoDark: imageJSON(t, consolebranding.TypeSVG, svgBytes),
	})
	require.NoError(t, err)
	assert.Equal(t, stored.Logo, rec.Logo, "absent keeps the stored image")
	assert.Nil(t, rec.Favicon, "null removes it")
	require.NotNil(t, rec.LogoDark, "an object sets it")
	assert.Equal(t, svgBytes, rec.LogoDark.Data)
}

func TestApply_Images(t *testing.T) {
	tests := []struct {
		name   string
		field  string
		req    func(json.RawMessage) operatorv1.PutConsoleBrandingRequest
		img    json.RawMessage
		reason string
	}{
		{"png ok", "", logoReq, imageJSON(t, consolebranding.TypePNG, pngBytes), ""},
		{"webp ok", "", logoReq, imageJSON(t, consolebranding.TypeWebP, webpBytes), ""},
		{"svg ok", "", logoReq, imageJSON(t, consolebranding.TypeSVG, svgBytes), ""},
		{"type does not match bytes", "logo", logoReq, imageJSON(t, consolebranding.TypePNG, svgBytes), "not a PNG"},
		{"webp header wrong", "logo", logoReq, imageJSON(t, consolebranding.TypeWebP, pngBytes), "not a WebP"},
		{"svg with html root", "logo", logoReq, imageJSON(t, consolebranding.TypeSVG, []byte(`<html><script>alert(1)</script></html>`)), "<svg> root"},
		{"svg with two roots", "logo", logoReq, imageJSON(t, consolebranding.TypeSVG, []byte(`<svg/><svg/>`)), "<svg> root"},
		{"svg not xml", "logo", logoReq, imageJSON(t, consolebranding.TypeSVG, []byte(`<svg><g></svg>`)), "<svg> root"},
		{"unsupported type", "logo", logoReq, imageJSON(t, "image/gif", []byte("GIF89a")), "content type"},
		{"empty data", "logo", logoReq, imageJSON(t, consolebranding.TypePNG, nil), "no data"},
		{"not an object", "logo", logoReq, json.RawMessage(`"x"`), "null or an object"},
		{"logo over 128 KiB", "logo", logoReq, imageJSON(t, consolebranding.TypePNG, padded(pngBytes, consolebranding.MaxLogoBytes+1)), "at most 131072"},
		{"favicon over 32 KiB", "favicon", faviconReq, imageJSON(t, consolebranding.TypePNG, padded(pngBytes, consolebranding.MaxFaviconBytes+1)), "at most 32768"},
		{"favicon at 32 KiB", "", faviconReq, imageJSON(t, consolebranding.TypePNG, padded(pngBytes, consolebranding.MaxFaviconBytes)), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := consolebranding.Apply(nil, tc.req(tc.img))
			if tc.field == "" {
				require.NoError(t, err)
				return
			}
			requireInvalid(t, err, tc.field, tc.reason)
		})
	}
}

// Every image at its own cap still fits the stored cap, which keeps the value
// under the NATS message size, and the request under the 720 KiB body cap.
func TestApply_LargestRecordFitsCaps(t *testing.T) {
	logo := imageJSON(t, consolebranding.TypePNG, padded(pngBytes, consolebranding.MaxLogoBytes))
	favicon := imageJSON(t, consolebranding.TypePNG, padded(pngBytes, consolebranding.MaxFaviconBytes))
	req := operatorv1.PutConsoleBrandingRequest{
		ProductName: strings.Repeat("a", 40),
		DocsURL:     ptr("https://docs.acme.example/" + strings.Repeat("a", 2000)),
		Logo:        logo, LogoDark: logo, Favicon: favicon,
	}
	rec, err := consolebranding.Apply(nil, req)
	require.NoError(t, err)

	stored, err := json.Marshal(rec)
	require.NoError(t, err)
	assert.Less(t, len(stored), consolebranding.MaxRecordBytes)
	body, err := json.Marshal(req)
	require.NoError(t, err)
	assert.Less(t, len(body), 720<<10)
}

func TestChangedFields_NamesFieldsNotValues(t *testing.T) {
	before := &consolebranding.Record{ProductName: "Old", Logo: &operatorv1.BrandingImage{ContentType: consolebranding.TypePNG, Data: pngBytes}}
	after := consolebranding.Record{ProductName: "Secret Name", DocsURL: ptr(""), Logo: before.Logo}

	got := consolebranding.ChangedFields(before, after)

	assert.Equal(t, []string{"productName", "docsUrl"}, got)
}

func TestStylesheet(t *testing.T) {
	assert.Empty(t, consolebranding.Stylesheet(operatorv1.BrandingColors{}))
	assert.Equal(t, ":root:not(.dark){--primary:#0A4A8F;--ring:#0A4A8F}\n",
		consolebranding.Stylesheet(operatorv1.BrandingColors{Light: "#0A4A8F"}), "a light-only record leaves the dark rule out")
	assert.Equal(t, ":root:not(.dark){--primary:#0A4A8F;--ring:#0A4A8F}\n:root.dark{--primary:#7FB8F0;--ring:#7FB8F0}\n",
		consolebranding.Stylesheet(operatorv1.BrandingColors{Light: "#0A4A8F", Dark: "#7FB8F0"}))
	assert.Empty(t, consolebranding.Stylesheet(operatorv1.BrandingColors{Light: "red}*{background:url(x)"}))
}

var cssRuleRE = regexp.MustCompile(`^(:root:not\(\.dark\)|:root\.dark)\{--primary:#[0-9A-Fa-f]{6};--ring:#[0-9A-Fa-f]{6}\}$`)

// No accepted record yields CSS beyond --primary and --ring in the two theme
// selectors, whatever the colour strings hold.
func FuzzStylesheetOnlyThemeRules(f *testing.F) {
	f.Add("#0A4A8F", "#7FB8F0")
	f.Add("#fff;}", "}body{x:url(y)")
	f.Fuzz(func(t *testing.T, light, dark string) {
		rec, err := consolebranding.Apply(nil, operatorv1.PutConsoleBrandingRequest{Primary: operatorv1.BrandingColors{Light: light, Dark: dark}})
		if err != nil {
			return
		}
		for line := range strings.SplitSeq(strings.TrimSuffix(consolebranding.Stylesheet(rec.Primary), "\n"), "\n") {
			if line == "" {
				continue
			}
			if !cssRuleRE.MatchString(line) {
				t.Fatalf("unexpected CSS %q", line)
			}
		}
	})
}

var oklchDeclRE = regexp.MustCompile(`^\s*(--[a-z-]+):\s*oklch\(([\d.]+)%\s+([\d.]+)\s+([\d.]+)\)`)

// The colour check draws against copies of the console's theme tokens; they
// must match styles.css or a theme change would leave the check out of date.
func TestThemeTokensMatchStylesheet(t *testing.T) {
	css, err := os.ReadFile("../../runtime/roles/spinifexui/frontend/src/styles.css")
	require.NoError(t, err)

	blocks := map[string][]byte{}
	light, rest, ok := bytes.Cut(css, []byte("\n.dark {"))
	require.True(t, ok, "styles.css has no .dark block")
	_, light, ok = bytes.Cut(light, []byte("\n:root {"))
	require.True(t, ok, "styles.css has no :root block")
	dark, _, _ := bytes.Cut(rest, []byte("\n}"))
	blocks["light"], blocks["dark"] = light, dark

	for themeName, tokens := range consolebranding.ThemeTokens() {
		declared := map[string][3]float64{}
		for line := range strings.SplitSeq(string(blocks[themeName]), "\n") {
			m := oklchDeclRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			l, _ := strconv.ParseFloat(m[2], 64)
			c, _ := strconv.ParseFloat(m[3], 64)
			h, _ := strconv.ParseFloat(m[4], 64)
			declared[m[1]] = [3]float64{l / 100, c, h}
		}
		for name, want := range tokens {
			got, ok := declared[name]
			require.True(t, ok, "%s %s is not an oklch() declaration in styles.css", themeName, name)
			for i := range want {
				assert.InDelta(t, want[i], got[i], 1e-9, "%s %s", themeName, name)
			}
		}
	}
}

func logoReq(img json.RawMessage) operatorv1.PutConsoleBrandingRequest {
	return operatorv1.PutConsoleBrandingRequest{Logo: img}
}

func faviconReq(img json.RawMessage) operatorv1.PutConsoleBrandingRequest {
	return operatorv1.PutConsoleBrandingRequest{Favicon: img}
}

func padded(head []byte, n int) []byte {
	out := make([]byte, n)
	copy(out, head)
	return out
}
