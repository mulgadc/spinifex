// Package consolebranding owns the cluster-wide console branding record: its
// validation, its store, the gateway's in-memory copy and the public routes
// the console loads it from.
package consolebranding

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"unicode"
	"unicode/utf8"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
)

// SchemaVersion is the version of the stored record.
const SchemaVersion = 1

// Size limits, in decoded bytes. The record total stays under the NATS
// default 1 MiB message size.
const (
	MaxLogoBytes      = 128 << 10
	MaxFaviconBytes   = 32 << 10
	MaxRecordBytes    = 512 << 10
	maxProductNameLen = 40
	maxDocsURLLen     = 2048
)

// Image content types the console can show through <img> and as a favicon.
const (
	TypePNG  = "image/png"
	TypeWebP = "image/webp"
	TypeSVG  = "image/svg+xml"
)

// Record is the stored branding value. Every field but SchemaVersion is
// optional, and an absent field keeps the console's built-in value. A nil
// DocsURL keeps the built-in link; an empty one hides it.
type Record struct {
	SchemaVersion int                       `json:"schemaVersion"`
	ProductName   string                    `json:"productName,omitempty"`
	DocsURL       *string                   `json:"docsUrl,omitempty"`
	Primary       operatorv1.BrandingColors `json:"primary,omitzero"`
	Logo          *operatorv1.BrandingImage `json:"logo,omitempty"`
	LogoDark      *operatorv1.BrandingImage `json:"logoDark,omitempty"`
	Favicon       *operatorv1.BrandingImage `json:"favicon,omitempty"`
	UpdatedBy     string                    `json:"updatedBy,omitempty"`
	UpdatedAt     string                    `json:"updatedAt,omitempty"`
}

// ValidationError names the field a request got wrong. Its message is safe to
// return to the caller.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Reason }

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// Apply builds the record a PutConsoleBranding request asks for, from the
// stored one. Images absent from the request are kept; the rest is replaced.
func Apply(stored *Record, req operatorv1.PutConsoleBrandingRequest) (Record, error) {
	next := Record{
		SchemaVersion: SchemaVersion,
		ProductName:   req.ProductName,
		DocsURL:       req.DocsURL,
		Primary:       req.Primary,
	}
	if stored == nil {
		stored = &Record{}
	}
	var err error
	if next.Logo, err = applyImage("logo", stored.Logo, req.Logo, MaxLogoBytes); err != nil {
		return Record{}, err
	}
	if next.LogoDark, err = applyImage("logoDark", stored.LogoDark, req.LogoDark, MaxLogoBytes); err != nil {
		return Record{}, err
	}
	if next.Favicon, err = applyImage("favicon", stored.Favicon, req.Favicon, MaxFaviconBytes); err != nil {
		return Record{}, err
	}
	if err := next.validate(); err != nil {
		return Record{}, err
	}
	return next, nil
}

// applyImage resolves one tri-state image field: absent keeps the stored
// image, null removes it and an object replaces it.
func applyImage(field string, stored *operatorv1.BrandingImage, raw json.RawMessage, limit int) (*operatorv1.BrandingImage, error) {
	if len(raw) == 0 {
		return stored, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var img operatorv1.BrandingImage
	if err := json.Unmarshal(raw, &img); err != nil {
		return nil, invalid(field, "must be null or an object with contentType and base64 data")
	}
	if err := checkImage(field, img, limit); err != nil {
		return nil, err
	}
	return &img, nil
}

func (r Record) validate() error {
	if r.ProductName != "" {
		if n := utf8.RuneCountInString(r.ProductName); n > maxProductNameLen {
			return invalid("productName", "is %d characters; at most %d are allowed", n, maxProductNameLen)
		}
		for _, c := range r.ProductName {
			if unicode.IsControl(c) {
				return invalid("productName", "must not contain control characters")
			}
		}
	}
	if r.DocsURL != nil && *r.DocsURL != "" {
		if err := checkDocsURL(*r.DocsURL); err != nil {
			return err
		}
	}
	for _, p := range []struct {
		field, value string
		theme        theme
	}{
		{"primary.light", r.Primary.Light, lightTheme},
		{"primary.dark", r.Primary.Dark, darkTheme},
	} {
		if p.value == "" {
			continue
		}
		c, ok := parseHex(p.value)
		if !ok {
			return invalid(p.field, "must be a #RRGGBB colour")
		}
		if err := checkPrimary(p.theme, c); err != nil {
			return invalid(p.field, "%s", err)
		}
	}
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode branding record: %w", err)
	}
	if len(data) > MaxRecordBytes {
		return invalid("record", "is %d bytes stored; at most %d are allowed", len(data), MaxRecordBytes)
	}
	return nil
}

func checkDocsURL(raw string) error {
	if len(raw) > maxDocsURLLen {
		return invalid("docsUrl", "is %d characters; at most %d are allowed", len(raw), maxDocsURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return invalid("docsUrl", "must be an https URL with a host and no user information, or empty to hide the link")
	}
	return nil
}

func checkImage(field string, img operatorv1.BrandingImage, limit int) error {
	if len(img.Data) == 0 {
		return invalid(field, "has no data")
	}
	if len(img.Data) > limit {
		return invalid(field, "is %d bytes; at most %d are allowed", len(img.Data), limit)
	}
	switch img.ContentType {
	case TypePNG:
		if !bytes.HasPrefix(img.Data, []byte("\x89PNG\r\n\x1a\n")) {
			return invalid(field, "is not a PNG image")
		}
	case TypeWebP:
		if len(img.Data) < 12 || string(img.Data[:4]) != "RIFF" || string(img.Data[8:12]) != "WEBP" {
			return invalid(field, "is not a WebP image")
		}
	case TypeSVG:
		if !isSVG(img.Data) {
			return invalid(field, "is not an SVG document with an <svg> root")
		}
	default:
		return invalid(field, "content type must be %s, %s or %s", TypePNG, TypeWebP, TypeSVG)
	}
	return nil
}

// isSVG reports whether data parses as XML whose root element is <svg>.
func isSVG(data []byte) bool {
	dec := xml.NewDecoder(bytes.NewReader(data))
	root := ""
	depth := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return root == "svg" && depth == 0
		}
		if err != nil {
			return false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				if root != "" {
					return false
				}
				root = t.Name.Local
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
}

// Response describes the record for /admin callers, without image bytes.
func (r *Record) Response(enabled bool, revision uint64) operatorv1.ConsoleBrandingResponse {
	resp := operatorv1.ConsoleBrandingResponse{Enabled: enabled, Revision: revision}
	if r == nil {
		return resp
	}
	info := func(img *operatorv1.BrandingImage) *operatorv1.BrandingImageInfo {
		if img == nil {
			return nil
		}
		return &operatorv1.BrandingImageInfo{ContentType: img.ContentType, Size: len(img.Data)}
	}
	resp.ProductName = r.ProductName
	resp.DocsURL = r.DocsURL
	resp.Primary = r.Primary
	resp.Logo = info(r.Logo)
	resp.LogoDark = info(r.LogoDark)
	resp.Favicon = info(r.Favicon)
	resp.UpdatedBy = r.UpdatedBy
	resp.UpdatedAt = r.UpdatedAt
	return resp
}

// ChangedFields names the fields that differ between two records, for the
// audit log line. It never carries a value.
func ChangedFields(before *Record, after Record) []string {
	if before == nil {
		before = &Record{}
	}
	imgEq := func(a, b *operatorv1.BrandingImage) bool {
		if a == nil || b == nil {
			return a == b
		}
		return a.ContentType == b.ContentType && bytes.Equal(a.Data, b.Data)
	}
	strPtrEq := func(a, b *string) bool {
		if a == nil || b == nil {
			return a == b
		}
		return *a == *b
	}
	var changed []string
	for _, f := range []struct {
		name string
		same bool
	}{
		{"productName", before.ProductName == after.ProductName},
		{"docsUrl", strPtrEq(before.DocsURL, after.DocsURL)},
		{"primary.light", before.Primary.Light == after.Primary.Light},
		{"primary.dark", before.Primary.Dark == after.Primary.Dark},
		{"logo", imgEq(before.Logo, after.Logo)},
		{"logoDark", imgEq(before.LogoDark, after.LogoDark)},
		{"favicon", imgEq(before.Favicon, after.Favicon)},
	} {
		if !f.same {
			changed = append(changed, f.name)
		}
	}
	return changed
}
