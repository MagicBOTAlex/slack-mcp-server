package text

import (
	"strings"
	"testing"
)

func TestUnitHTMLToText(t *testing.T) {
	raw := `<div class="quip-canvas-content"><h1>Huddle notes</h1><p>First line<br>second line</p><ul><li>one</li><li>two</li></ul></div>`
	got := HTMLToText(raw)
	want := "Huddle notes\n\nFirst line\nsecond line\n\n- one\n\n- two"
	if got != want {
		t.Fatalf("HTMLToText() = %q, want %q", got, want)
	}
	if !strings.Contains(got, "Huddle notes") {
		t.Fatalf("expected heading in output, got %q", got)
	}
}
