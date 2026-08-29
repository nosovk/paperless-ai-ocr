package ocr

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

func TestValidateAcceptsExactOrderedPages(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		firstPage int
		lastPage  int
		want      []Page
	}{
		{
			name:      "single page preserves whitespace and newlines",
			raw:       `{"pages":[{"page":7,"text":"  Heading\r\nbody\n\n","refused":false}]}`,
			firstPage: 7,
			lastPage:  7,
			want:      []Page{{number: 7, text: "  Heading\r\nbody\n\n"}},
		},
		{
			name:      "multiple pages",
			raw:       `{"pages":[{"page":2,"text":"two","refused":false},{"page":3,"text":"three","refused":false}]}`,
			firstPage: 2,
			lastPage:  3,
			want:      []Page{{number: 2, text: "two"}, {number: 3, text: "three"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			batch, err := Validate([]byte(test.raw), test.firstPage, test.lastPage)
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if got := batch.Pages(); !equalPages(got, test.want) {
				t.Errorf("Pages() = %#v, want %#v", got, test.want)
			}
			if batch.FirstPage() != test.firstPage || batch.LastPage() != test.lastPage {
				t.Errorf("batch range = %d-%d, want %d-%d", batch.FirstPage(), batch.LastPage(), test.firstPage, test.lastPage)
			}

			pages := batch.Pages()
			pages[0].text = "mutated"
			if equalPages(batch.Pages(), pages) {
				t.Error("Pages() exposed mutable batch storage")
			}
		})
	}
}

func TestValidateRejectsInvalidOutput(t *testing.T) {
	invalidUTF8 := append([]byte(`{"pages":[{"page":1,"text":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`","refused":false}]}`)...)
	tests := []struct {
		name      string
		raw       []byte
		firstPage int
		lastPage  int
	}{
		{name: "nil output", raw: nil, firstPage: 1, lastPage: 1},
		{name: "invalid range zero", raw: []byte(`{}`), firstPage: 0, lastPage: 1},
		{name: "invalid range reversed", raw: []byte(`{}`), firstPage: 2, lastPage: 1},
		{name: "range overflow", raw: []byte(`{}`), firstPage: 1, lastPage: int(^uint(0) >> 1)},
		{name: "invalid json", raw: []byte(`{"pages":`), firstPage: 1, lastPage: 1},
		{name: "trailing json", raw: []byte(`{"pages":[]} {}`), firstPage: 1, lastPage: 1},
		{name: "trailing prose", raw: []byte(`{"pages":[]} CANARY`), firstPage: 1, lastPage: 1},
		{name: "unknown top level field", raw: []byte(`{"pages":[{"page":1,"text":"one","refused":false}],"extra":"CANARY"}`), firstPage: 1, lastPage: 1},
		{name: "unknown page field", raw: []byte(`{"pages":[{"page":1,"text":"one","refused":false,"extra":"CANARY"}]}`), firstPage: 1, lastPage: 1},
		{name: "duplicate pages field", raw: []byte(`{"pages":[{"page":1,"text":"one","refused":false}],"pages":[{"page":1,"text":"again","refused":false}]}`), firstPage: 1, lastPage: 1},
		{name: "duplicate page number field", raw: []byte(`{"pages":[{"page":1,"page":1,"text":"one","refused":false}]}`), firstPage: 1, lastPage: 1},
		{name: "duplicate text field", raw: []byte(`{"pages":[{"page":1,"text":"one","text":"again","refused":false}]}`), firstPage: 1, lastPage: 1},
		{name: "duplicate refused field", raw: []byte(`{"pages":[{"page":1,"text":"one","refused":false,"refused":false}]}`), firstPage: 1, lastPage: 1},
		{name: "missing pages", raw: []byte(`{}`), firstPage: 1, lastPage: 1},
		{name: "null pages", raw: []byte(`{"pages":null}`), firstPage: 1, lastPage: 1},
		{name: "wrong pages type", raw: []byte(`{"pages":{}}`), firstPage: 1, lastPage: 1},
		{name: "missing page", raw: []byte(`{"pages":[{"text":"one","refused":false}]}`), firstPage: 1, lastPage: 1},
		{name: "missing text", raw: []byte(`{"pages":[{"page":1,"refused":false}]}`), firstPage: 1, lastPage: 1},
		{name: "missing refused", raw: []byte(`{"pages":[{"page":1,"text":"one"}]}`), firstPage: 1, lastPage: 1},
		{name: "wrong page type", raw: []byte(`{"pages":[{"page":"1","text":"one","refused":false}]}`), firstPage: 1, lastPage: 1},
		{name: "fractional page", raw: []byte(`{"pages":[{"page":1.0,"text":"one","refused":false}]}`), firstPage: 1, lastPage: 1},
		{name: "wrong text type", raw: []byte(`{"pages":[{"page":1,"text":1,"refused":false}]}`), firstPage: 1, lastPage: 1},
		{name: "null text", raw: []byte(`{"pages":[{"page":1,"text":null,"refused":false}]}`), firstPage: 1, lastPage: 1},
		{name: "wrong refused type", raw: []byte(`{"pages":[{"page":1,"text":"one","refused":"false"}]}`), firstPage: 1, lastPage: 1},
		{name: "missing requested page", raw: []byte(`{"pages":[{"page":1,"text":"one","refused":false}]}`), firstPage: 1, lastPage: 2},
		{name: "extra page", raw: []byte(`{"pages":[{"page":1,"text":"one","refused":false},{"page":2,"text":"two","refused":false}]}`), firstPage: 1, lastPage: 1},
		{name: "duplicate page", raw: []byte(`{"pages":[{"page":1,"text":"one","refused":false},{"page":1,"text":"again","refused":false}]}`), firstPage: 1, lastPage: 2},
		{name: "unordered pages", raw: []byte(`{"pages":[{"page":2,"text":"two","refused":false},{"page":1,"text":"one","refused":false}]}`), firstPage: 1, lastPage: 2},
		{name: "gap", raw: []byte(`{"pages":[{"page":1,"text":"one","refused":false},{"page":3,"text":"three","refused":false}]}`), firstPage: 1, lastPage: 2},
		{name: "below range", raw: []byte(`{"pages":[{"page":1,"text":"one","refused":false}]}`), firstPage: 2, lastPage: 2},
		{name: "above range", raw: []byte(`{"pages":[{"page":3,"text":"three","refused":false}]}`), firstPage: 2, lastPage: 2},
		{name: "blank text", raw: []byte(`{"pages":[{"page":1,"text":" \r\n\t","refused":false}]}`), firstPage: 1, lastPage: 1},
		{name: "refused", raw: []byte(`{"pages":[{"page":1,"text":"","refused":true}]}`), firstPage: 1, lastPage: 1},
		{name: "invalid utf8", raw: invalidUTF8, firstPage: 1, lastPage: 1},
		{name: "excessive raw output", raw: []byte(strings.Repeat(" ", maxRawBatchBytes+1)), firstPage: 1, lastPage: 1},
		{name: "excessive page text", raw: transcriptionJSON(1, strings.Repeat("x", maxPageTextBytes+1)), firstPage: 1, lastPage: 1},
		{name: "excessive batch text", raw: []byte(fmt.Sprintf(`{"pages":[{"page":1,"text":%q,"refused":false},{"page":2,"text":%q,"refused":false}]}`, strings.Repeat("a", maxBatchTextBytes/2+1), strings.Repeat("b", maxBatchTextBytes/2+1))), firstPage: 1, lastPage: 2},
		{name: "deeply nested unknown value", raw: []byte(`{"pages":[],"extra":[[[[[[[[[[[[[[[[[[[["CANARY"]]]]]]]]]]]]]]]]]]]]}`), firstPage: 1, lastPage: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Validate(test.raw, test.firstPage, test.lastPage)
			assertSafeProviderError(t, err, test.raw)
		})
	}
}

func TestValidateSuspiciousNonTranscriptionProse(t *testing.T) {
	for _, text := range []string{
		"I'm sorry, but I can't transcribe this document.",
		"I am sorry, but I cannot assist with that request.",
		"As an AI language model, I cannot view the attached pages.",
		"I cannot comply with this request.",
	} {
		t.Run(text, func(t *testing.T) {
			_, err := Validate(transcriptionJSON(1, text), 1, 1)
			assertSafeProviderError(t, err, []byte(text))
		})
	}
}

func TestValidateAllowsSimilarDocumentText(t *testing.T) {
	for _, text := range []string{
		"SUMMARY\nThe committee cannot approve expenditures without a quorum.",
		"The witness stated: I cannot comply with this request. The hearing then adjourned.",
		"I'm sorry, but I can't transcribe this document was the exact message displayed by the machine, according to the report.",
		"As an AI language model, I cannot view the attached pages is an example sentence in this textbook chapter.",
	} {
		t.Run(text, func(t *testing.T) {
			if _, err := Validate(transcriptionJSON(1, text), 1, 1); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestJoinUsesStablePageSeparators(t *testing.T) {
	tests := []struct {
		name    string
		batches []Batch
		want    string
	}{
		{
			name:    "one page",
			batches: []Batch{mustValidate(t, `{"pages":[{"page":1,"text":"one","refused":false}]}`, 1, 1)},
			want:    "----- PAGE 1; BYTES 3 -----\n\none",
		},
		{
			name:    "multiple pages",
			batches: []Batch{mustValidate(t, `{"pages":[{"page":1,"text":"one\n","refused":false},{"page":2,"text":" two ","refused":false}]}`, 1, 2)},
			want:    "----- PAGE 1; BYTES 4 -----\n\none\n\n----- PAGE 2; BYTES 5 -----\n\n two ",
		},
		{
			name: "cross batch pages",
			batches: []Batch{
				mustValidate(t, `{"pages":[{"page":1,"text":"one","refused":false},{"page":2,"text":"two","refused":false}]}`, 1, 2),
				mustValidate(t, `{"pages":[{"page":3,"text":"three","refused":false}]}`, 3, 3),
			},
			want: "----- PAGE 1; BYTES 3 -----\n\none\n\n----- PAGE 2; BYTES 3 -----\n\ntwo\n\n----- PAGE 3; BYTES 5 -----\n\nthree",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := snapshotBatches(test.batches)
			got, err := Join(test.batches)
			if err != nil {
				t.Fatalf("Join() error = %v", err)
			}
			if got != test.want {
				t.Errorf("Join() = %q, want %q", got, test.want)
			}
			if after := snapshotBatches(test.batches); after != before {
				t.Error("Join() mutated its inputs")
			}
		})
	}
}

func TestJoinRejectsInvalidBatches(t *testing.T) {
	page1 := mustValidate(t, `{"pages":[{"page":1,"text":"one","refused":false}]}`, 1, 1)
	page2 := mustValidate(t, `{"pages":[{"page":2,"text":"two","refused":false}]}`, 2, 2)
	page3 := mustValidate(t, `{"pages":[{"page":3,"text":"three","refused":false}]}`, 3, 3)
	largeText := strings.Repeat("x", maxPageTextBytes)
	largeBatch := mustValidate(t, string(transcriptionJSON(1, largeText)), 1, 1)
	tests := []struct {
		name    string
		batches []Batch
	}{
		{name: "nil", batches: nil},
		{name: "empty", batches: []Batch{}},
		{name: "zero batch", batches: []Batch{{}}},
		{name: "does not start at page one", batches: []Batch{page2}},
		{name: "duplicate batch", batches: []Batch{page1, page1}},
		{name: "duplicate page across batches", batches: []Batch{mustValidate(t, `{"pages":[{"page":1,"text":"one","refused":false},{"page":2,"text":"two","refused":false}]}`, 1, 2), page2}},
		{name: "gapped batches", batches: []Batch{page1, page3}},
		{name: "unordered batches", batches: []Batch{page2, page1}},
		{name: "invalid internal range", batches: []Batch{{firstPage: 1, lastPage: 2, pages: []Page{{number: 1, text: "one"}}}}},
		{name: "invalid internal text", batches: []Batch{{firstPage: 1, lastPage: 1, pages: []Page{{number: 1}}}}},
		{name: "excessive joined output", batches: repeatedBatches(largeBatch, maxJoinedContentBytes/maxPageTextBytes+1)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Join(test.batches)
			assertSafeProviderError(t, err, nil)
		})
	}
}

func FuzzValidate(f *testing.F) {
	f.Add([]byte(`{"pages":[{"page":1,"text":"CANARY-one","refused":false}]}`), 1, 1)
	f.Add([]byte(`{"pages":[]} trailing-CANARY`), 1, 1)
	f.Add([]byte{0xff, 'C', 'A', 'N', 'A', 'R', 'Y'}, 1, 1)
	f.Fuzz(func(t *testing.T, raw []byte, firstPage, lastPage int) {
		batch1, err1 := Validate(raw, firstPage, lastPage)
		batch2, err2 := Validate(raw, firstPage, lastPage)
		if fmt.Sprint(err1) != fmt.Sprint(err2) || snapshotBatches([]Batch{batch1}) != snapshotBatches([]Batch{batch2}) {
			t.Fatal("Validate() is not deterministic")
		}
		if err1 != nil {
			assertSafeProviderError(t, err1, raw)
		}
	})
}

func FuzzJoin(f *testing.F) {
	f.Add(1, "CANARY-one")
	f.Add(2, "two\nlines")
	f.Fuzz(func(t *testing.T, page int, text string) {
		if page <= 0 || len(text) > maxPageTextBytes || !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
			return
		}
		batch := Batch{firstPage: page, lastPage: page, pages: []Page{{number: page, text: text}}}
		got1, err1 := Join([]Batch{batch})
		got2, err2 := Join([]Batch{batch})
		if got1 != got2 || fmt.Sprint(err1) != fmt.Sprint(err2) {
			t.Fatal("Join() is not deterministic")
		}
		if err1 != nil {
			assertSafeProviderError(t, err1, []byte(text))
		}
	})
}

func assertSafeProviderError(t *testing.T, err error, _ []byte) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want provider validation error")
	}
	var safeError *saferr.Error
	if !errors.As(err, &safeError) || safeError.Category() != saferr.CategoryProvider {
		t.Fatalf("error = %T %v, want saferr.CategoryProvider", err, err)
	}
	if got := err.Error(); got != "provider: "+invalidTranscriptionMessage && got != "provider: "+invalidJoinMessage {
		t.Errorf("error = %q, want stable generic message", got)
	}
	for _, formatted := range []string{fmt.Sprintf("%s", err), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		if strings.Contains(formatted, "CANARY") {
			t.Errorf("error disclosed input marker: %q", formatted)
		}
	}
	for cause := errors.Unwrap(err); cause != nil; cause = errors.Unwrap(cause) {
		formatted := fmt.Sprintf("%+v", cause)
		if strings.Contains(formatted, "CANARY") {
			t.Errorf("unwrap chain disclosed input: %q", formatted)
		}
	}
}

func equalPages(left, right []Page) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Number() != right[index].Number() || left[index].Text() != right[index].Text() {
			return false
		}
	}
	return true
}

func mustValidate(t *testing.T, raw string, firstPage, lastPage int) Batch {
	t.Helper()
	batch, err := Validate([]byte(raw), firstPage, lastPage)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	return batch
}

func transcriptionJSON(page int, text string) []byte {
	return []byte(fmt.Sprintf(`{"pages":[{"page":%d,"text":%q,"refused":false}]}`, page, text))
}

func snapshotBatches(batches []Batch) string {
	var snapshot strings.Builder
	for _, batch := range batches {
		fmt.Fprintf(&snapshot, "%d:%d:", batch.FirstPage(), batch.LastPage())
		for _, page := range batch.Pages() {
			fmt.Fprintf(&snapshot, "%d:%q;", page.Number(), page.Text())
		}
	}
	return snapshot.String()
}

func repeatedBatches(batch Batch, count int) []Batch {
	batches := make([]Batch, count)
	for index := range batches {
		page := index + 1
		batches[index] = Batch{firstPage: page, lastPage: page, pages: []Page{{number: page, text: batch.pages[0].text}}}
	}
	return batches
}
