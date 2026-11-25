package operatorv1

import "encoding/json"

// BrandingColors is the console's primary colour for each theme, as #RRGGBB.
// An empty value keeps the built-in colour for that theme.
type BrandingColors struct {
	Light string `json:"light,omitempty"`
	Dark  string `json:"dark,omitempty"`
}

// BrandingImage is one uploaded image. Data is base64 on the wire.
type BrandingImage struct {
	ContentType string `json:"contentType"`
	Data        []byte `json:"data"`
}

// BrandingImageInfo describes a stored image without its bytes.
type BrandingImageInfo struct {
	ContentType string `json:"contentType"`
	Size        int    `json:"size"`
}

// PutConsoleBrandingRequest replaces the branding record; the last save wins.
// Each image field is tri-state: absent keeps the stored image, null removes
// it, and an object sets it.
type PutConsoleBrandingRequest struct {
	ProductName string          `json:"productName,omitempty"`
	DocsURL     *string         `json:"docsUrl,omitempty"`
	Primary     BrandingColors  `json:"primary,omitzero"`
	Logo        json.RawMessage `json:"logo,omitempty"`
	LogoDark    json.RawMessage `json:"logoDark,omitempty"`
	Favicon     json.RawMessage `json:"favicon,omitempty"`
}

// ConsoleBrandingResponse is the /admin/GetConsoleBranding and
// /admin/PutConsoleBranding success body. A nil DocsURL is the built-in link
// and an empty one hides it.
type ConsoleBrandingResponse struct {
	Revision    uint64             `json:"revision"`
	ProductName string             `json:"productName,omitempty"`
	DocsURL     *string            `json:"docsUrl,omitempty"`
	Primary     BrandingColors     `json:"primary,omitzero"`
	Logo        *BrandingImageInfo `json:"logo,omitempty"`
	LogoDark    *BrandingImageInfo `json:"logoDark,omitempty"`
	Favicon     *BrandingImageInfo `json:"favicon,omitempty"`
	UpdatedBy   string             `json:"updatedBy,omitempty"`
	UpdatedAt   string             `json:"updatedAt,omitempty"`
}
