// Package assets holds the project's artwork, embedded in the binary.
//
// It lives at the repository root so one copy serves both the README and the
// management interface. Go's embed directive cannot reach outside its own
// package directory, so a copy under internal/ would have meant either a second
// file to keep in step or a README pointing at an oddly deep path.
package assets

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
)

// Icon is the application icon: shown in the browser tab, beside the title in
// the management interface, and at the top of the README.
//
//go:embed icon.svg
var Icon []byte

// DevMark is the author's mark, shown beside their name in the interface
// footer. Its strokes use currentColor, so it inherits the surrounding text
// colour in either theme.
//
//go:embed dev-mark.svg
var DevMark []byte

// SVGContentType is the media type both assets are served with.
const SVGContentType = "image/svg+xml"

// ETag returns a strong validator for an asset, so a browser can revalidate it
// across reloads and a new build always wins.
func ETag(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + base64.RawURLEncoding.EncodeToString(sum[:8]) + `"`
}
