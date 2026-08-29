package ocr

import (
	"strings"
	"testing"
)

func TestPromptIsVersionedAndRequiresFaithfulPageTranscription(t *testing.T) {
	const draft = "CANARY untrusted native OCR draft"
	prompt := Prompt(7, 9, draft)

	if Version == "" {
		t.Fatal("Version is blank")
	}
	for _, required := range []string{
		"untrusted evidence",
		"Do not summarize",
		"Do not translate",
		"Do not normalize",
		"Do not invent",
		"Do not infer",
		"Do not omit",
		"pages 7 through 9",
		"page",
		"text",
		"refused",
		draft,
	} {
		if !strings.Contains(prompt, required) {
			t.Errorf("Prompt() does not contain %q", required)
		}
	}
}

func TestPromptRejectsInvalidPageRange(t *testing.T) {
	for _, pages := range [][2]int{{0, 1}, {1, 0}, {2, 1}} {
		if got := Prompt(pages[0], pages[1], "draft"); got != "" {
			t.Errorf("Prompt(%d, %d, draft) = %q, want blank", pages[0], pages[1], got)
		}
	}
}
