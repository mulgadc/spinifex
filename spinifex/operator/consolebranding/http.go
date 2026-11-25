package consolebranding

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
)

// PathPrefix is where the gateway serves the public branding routes.
const PathPrefix = "/console/branding/"

// SVG script never runs through <img>; this stops it when a user opens the
// image URL directly.
const svgCSP = "default-src 'none'; style-src 'unsafe-inline'; sandbox"

const immutableCache = "public, max-age=31536000, immutable"

// Public is the console's view of the branding. Asset paths are relative to
// the branding route and carry the revision, so a change gives new URLs.
type Public struct {
	Enabled     bool    `json:"enabled"`
	Revision    uint64  `json:"revision,omitempty"`
	ProductName string  `json:"productName,omitempty"`
	DocsURL     *string `json:"docsUrl,omitempty"`
	Stylesheet  string  `json:"stylesheet,omitempty"`
	Logo        string  `json:"logo,omitempty"`
	LogoDark    string  `json:"logoDark,omitempty"`
	Favicon     string  `json:"favicon,omitempty"`
}

// Handler serves the public branding routes from the cache. It carries
// nothing a tenant cannot see after login.
type Handler struct {
	Enabled bool
	Cache   *Cache
}

// ServeHTTP serves branding.json, branding.css and the three images.
func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, PathPrefix)
	if name == "branding.json" {
		h.serveJSON(w)
		return
	}

	rec, rev := h.Cache.Snapshot()
	if !h.Enabled || rec == nil || r.URL.Query().Get("rev") != strconv.FormatUint(rev, 10) {
		http.NotFound(w, r)
		return
	}
	switch name {
	case "branding.css":
		css := Stylesheet(rec.Primary)
		if css == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", immutableCache)
		_, _ = w.Write([]byte(css))
	case "logo":
		serveImage(w, r, rec.Logo)
	case "logo-dark":
		serveImage(w, r, rec.LogoDark)
	case "favicon":
		serveImage(w, r, rec.Favicon)
	default:
		http.NotFound(w, r)
	}
}

func (h Handler) serveJSON(w http.ResponseWriter) {
	out := Public{Enabled: h.Enabled}
	if rec, rev := h.Cache.Snapshot(); h.Enabled && rec != nil {
		asset := func(name string, set bool) string {
			if !set {
				return ""
			}
			return name + "?rev=" + strconv.FormatUint(rev, 10)
		}
		out.Revision = rev
		out.ProductName = rec.ProductName
		out.DocsURL = rec.DocsURL
		out.Stylesheet = asset("branding.css", Stylesheet(rec.Primary) != "")
		out.Logo = asset("logo", rec.Logo != nil)
		out.LogoDark = asset("logo-dark", rec.LogoDark != nil)
		out.Favicon = asset("favicon", rec.Favicon != nil)
	}
	body, err := json.Marshal(out)
	if err != nil {
		slog.Error("Console branding: encode public view failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

func serveImage(w http.ResponseWriter, r *http.Request, img *operatorv1.BrandingImage) {
	if img == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", img.ContentType)
	w.Header().Set("Cache-Control", immutableCache)
	if img.ContentType == TypeSVG {
		w.Header().Set("Content-Security-Policy", svgCSP)
	}
	_, _ = w.Write(img.Data)
}

// Stylesheet returns the theme overrides for the set colours, or "" when none
// is set. Each rule applies only in its own theme, and only #RRGGBB values
// reach it, so the output cannot carry other CSS.
func Stylesheet(p operatorv1.BrandingColors) string {
	var b strings.Builder
	for _, t := range []struct{ selector, value string }{
		{":root:not(.dark)", p.Light},
		{":root.dark", p.Dark},
	} {
		if !validHex(t.value) {
			continue
		}
		fmt.Fprintf(&b, "%s{--primary:%s;--ring:%s}\n", t.selector, t.value, t.value)
	}
	return b.String()
}
