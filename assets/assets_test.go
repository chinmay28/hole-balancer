package assets

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestAssetsAreWellFormedSVG(t *testing.T) {
	for name, body := range map[string][]byte{"icon.svg": Icon, "dev-mark.svg": DevMark} {
		if len(body) == 0 {
			t.Fatalf("%s: embedded but empty", name)
		}
		// Malformed artwork renders as nothing, and a browser will not say why.
		if err := xml.Unmarshal(body, new(struct{})); err != nil {
			t.Errorf("%s: not well-formed XML: %v", name, err)
		}
		if !strings.Contains(string(body), "<svg") {
			t.Errorf("%s: not an SVG", name)
		}
	}
}

// The icon is a favicon, sitting on whatever colour the browser's tab strip
// happens to be, so it carries its own background and explicit colours. A mark
// relying on currentColor would be invisible on half of them.
func TestIconIsSelfColouring(t *testing.T) {
	body := string(Icon)
	if strings.Contains(body, "currentColor") {
		t.Error("icon must not depend on currentColor: nothing inherits into a favicon")
	}
	if !strings.Contains(body, "#2a78d6") {
		t.Error("icon should carry its own background fill")
	}
}

// The author's mark is inlined into the page and must take the surrounding text
// colour, so that one file works in both themes.
func TestDevMarkInheritsColour(t *testing.T) {
	body := string(DevMark)
	if !strings.Contains(body, "currentColor") {
		t.Error("dev mark should use currentColor so it works in both themes")
	}
	if strings.Contains(body, "<title>") {
		t.Error("dev mark should stay decorative: the visible name beside it is the label")
	}
}

func TestETagIsStableAndDistinct(t *testing.T) {
	if got, want := ETag(Icon), ETag(Icon); got != want {
		t.Error("ETag is not stable for the same content")
	}
	if ETag(Icon) == ETag(DevMark) {
		t.Error("different assets share an ETag")
	}
	if e := ETag(Icon); !strings.HasPrefix(e, `"`) || !strings.HasSuffix(e, `"`) {
		t.Errorf("ETag %s must be a quoted string per RFC 9110", e)
	}
}
