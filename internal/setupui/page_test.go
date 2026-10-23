package setupui

import (
	"regexp"
	"testing"
)

// The wizard loads nothing from elsewhere: no webfont, script or stylesheet tells a third party who set a bridge up.
func TestPageLoadsNothingFromElsewhere(t *testing.T) {
	for _, pattern := range []string{`<link\b`, `<script[^>]+src=`, `@import`, `url\(\s*["']?https?:`, `src=["']https?:`} {
		if loc := regexp.MustCompile(pattern).FindStringIndex(indexHTML); loc != nil {
			t.Errorf("index.html loads from elsewhere (%s): %q", pattern, indexHTML[loc[0]:min(loc[1]+40, len(indexHTML))])
		}
	}
}
