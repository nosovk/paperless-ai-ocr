package ocr

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

func TestBoundDraft(t *testing.T) {
	if got := BoundDraft(""); got != "" {
		t.Errorf("BoundDraft(empty) = %q, want empty", got)
	}
	short := "native text"
	if got := BoundDraft(short); got != short {
		t.Errorf("BoundDraft(short) = %q, want unchanged", got)
	}
	huge := strings.Repeat("x", MaxDraftBytes+100)
	got := BoundDraft(huge)
	if len(got) > MaxDraftBytes || !utf8.ValidString(got) || !strings.HasSuffix(got, draftTruncationMarker) {
		t.Errorf("BoundDraft(huge) length/UTF-8/marker = %d/%t/%t", len(got), utf8.ValidString(got), strings.HasSuffix(got, draftTruncationMarker))
	}
	boundary := strings.Repeat("x", MaxDraftBytes-len(draftTruncationMarker)-1) + "€" + strings.Repeat("secret", 20)
	got = BoundDraft(boundary)
	if !utf8.ValidString(got) || strings.Contains(got, "secret") || len(got) > MaxDraftBytes {
		t.Errorf("BoundDraft(UTF-8 boundary) valid/secret/length = %t/%t/%d", utf8.ValidString(got), strings.Contains(got, "secret"), len(got))
	}
}

func TestBoundDraftSanitizesInvalidUTF8BeforeBounding(t *testing.T) {
	tests := []struct {
		name  string
		draft string
		want  string
	}{
		{name: "invalid below limit", draft: string([]byte{'a', 0xff, 'b'}), want: "a\uFFFDb"},
		{name: "exact ASCII boundary", draft: strings.Repeat("x", MaxDraftBytes), want: strings.Repeat("x", MaxDraftBytes)},
		{name: "invalid above limit", draft: strings.Repeat("x", MaxDraftBytes) + string([]byte{0xff})},
		{name: "multibyte boundary", draft: strings.Repeat("x", MaxDraftBytes-len(draftTruncationMarker)-1) + "€tail"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := BoundDraft(test.draft)
			if !utf8.ValidString(got) || len(got) > MaxDraftBytes {
				t.Fatalf("BoundDraft() valid/length = %t/%d", utf8.ValidString(got), len(got))
			}
			if test.want != "" && got != test.want {
				t.Fatalf("BoundDraft() = %q, want %q", got, test.want)
			}
			if repeated := BoundDraft(test.draft); repeated != got {
				t.Fatalf("BoundDraft() is nondeterministic")
			}
		})
	}
}

func TestCanonicalValidate(t *testing.T) {
	raw := []byte(" { \n \"pages\" : [ { \"refused\" : false, \"text\" : \"one\", \"page\" : 1 } ] } \n")
	batch, canonical, err := ValidateCanonical(raw, 1, 1)
	if err != nil {
		t.Fatalf("ValidateCanonical() error = %v", err)
	}
	if batch.FirstPage() != 1 || string(canonical) != `{"pages":[{"page":1,"text":"one","refused":false}]}` {
		t.Errorf("ValidateCanonical() = (%+v, %s)", batch, canonical)
	}
}

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

func TestValidateRejectsCaseVariantSchemaKeys(t *testing.T) {
	templates := map[string]string{
		"pages":   `{"%s":[{"page":1,"text":"one","refused":false}]}`,
		"page":    `{"pages":[{"%s":1,"text":"one","refused":false}]}`,
		"text":    `{"pages":[{"page":1,"%s":"one","refused":false}]}`,
		"refused": `{"pages":[{"page":1,"text":"one","%s":false}]}`,
	}
	for key, template := range templates {
		for _, variant := range caseVariants(key) {
			t.Run(key+"_as_"+variant, func(t *testing.T) {
				raw := []byte(fmt.Sprintf(template, variant))
				_, err := Validate(raw, 1, 1)
				assertSafeProviderError(t, err, raw)
			})
		}
	}
}

func TestValidateRejectsMixedCaseSemanticDuplicateKeys(t *testing.T) {
	for _, raw := range []string{
		`{"pages":[{"page":1,"text":"one","refused":false}],"Pages":[{"page":1,"text":"again","refused":false}]}`,
		`{"Pages":[{"page":1,"text":"one","refused":false}],"pAgEs":[{"page":1,"text":"again","refused":false}]}`,
		`{"pages":[{"page":1,"Page":1,"text":"one","refused":false}]}`,
		`{"pages":[{"Page":1,"pAgE":1,"text":"one","refused":false}]}`,
		`{"pages":[{"page":1,"text":"one","Text":"again","refused":false}]}`,
		`{"pages":[{"page":1,"Text":"one","tExT":"again","refused":false}]}`,
		`{"pages":[{"page":1,"text":"one","refused":false,"Refused":false}]}`,
		`{"pages":[{"page":1,"text":"one","Refused":false,"rEfUsEd":false}]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := Validate([]byte(raw), 1, 1)
			assertSafeProviderError(t, err, []byte(raw))
		})
	}
}

func TestValidateRejectsUnicodeFoldedSchemaAliases(t *testing.T) {
	for _, raw := range []string{
		`{"pageſ":[{"page":1,"text":"one","refused":false}]}`,
		`{"page\u017f":[{"page":1,"text":"one","refused":false}]}`,
		`{"pages":[{"page":1,"text":"one","refuſed":false}]}`,
		`{"pages":[{"page":1,"text":"one","refu\u017fed":false}]}`,
		`{"pages":[{"page":1,"text":"one","refused":false}],"pageſ":[{"page":1,"text":"again","refused":false}]}`,
		`{"pageſ":[{"page":1,"text":"one","refused":false}],"pages":[{"page":1,"text":"again","refused":false}]}`,
		`{"pages":[{"page":1,"text":"one","refused":false,"refuſed":true}]}`,
		`{"pages":[{"page":1,"text":"one","refuſed":true,"refused":false}]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := Validate([]byte(raw), 1, 1)
			assertSafeProviderError(t, err, []byte(raw))
		})
	}
}

func TestValidateRejectsEscapedSchemaKeys(t *testing.T) {
	for _, key := range []string{"pages", "page", "text", "refused"} {
		for _, escaped := range escapedKeyVariants(key) {
			for _, test := range []struct {
				name string
				raw  string
			}{
				{name: "correct_level", raw: rawWithKeyAtCorrectLevel(key, escaped)},
				{name: "wrong_level", raw: rawWithKeyAtWrongLevel(key, escaped)},
				{name: "escaped_then_literal", raw: rawWithEscapedAndLiteralKeys(key, escaped, true)},
				{name: "literal_then_escaped", raw: rawWithEscapedAndLiteralKeys(key, escaped, false)},
			} {
				t.Run(key+"/"+escaped+"/"+test.name, func(t *testing.T) {
					_, err := Validate([]byte(test.raw), 1, 1)
					assertSafeProviderError(t, err, []byte(test.raw))
				})
			}
		}
	}
}

func TestValidateRejectsMalformedUnicodeEscapes(t *testing.T) {
	for _, raw := range []string{
		`{"pages":[{"page":1,"text":"lone high \ud800 marker","refused":false}]}`,
		`{"pages":[{"page":1,"text":"lone low \udc00 marker","refused":false}]}`,
		`{"pages":[{"page":1,"text":"high then text \ud800x","refused":false}]}`,
		`{"pages":[{"page":1,"text":"high then escape \ud800\n","refused":false}]}`,
		`{"pages":[{"page":1,"text":"reversed pair \udc00\ud800","refused":false}]}`,
		`{"pages":[{"page":1,"text":"wrong pair \ud800\ud800","refused":false}]}`,
		`{"pag\ud800es":[{"page":1,"text":"one","refused":false}]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := Validate([]byte(raw), 1, 1)
			assertSafeProviderError(t, err, []byte(raw))
		})
	}
}

func TestValidateAcceptsValidUnicodeReplacementAndSurrogatePair(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "valid surrogate pair", raw: `{"pages":[{"page":1,"text":"music \ud834\udd1e","refused":false}]}`, want: "music 𝄞"},
		{name: "literal replacement rune", raw: `{"pages":[{"page":1,"text":"literal � marker","refused":false}]}`, want: "literal � marker"},
		{name: "escaped replacement rune", raw: `{"pages":[{"page":1,"text":"escaped \ufffd marker","refused":false}]}`, want: "escaped � marker"},
	} {
		t.Run(test.name, func(t *testing.T) {
			batch, err := Validate([]byte(test.raw), 1, 1)
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if got := batch.Pages()[0].Text(); got != test.want {
				t.Errorf("Text() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestValidateAllowsRefusalLikeDocumentText(t *testing.T) {
	for _, text := range []string{
		"I'm sorry, but I can't transcribe this document.",
		"I am sorry, but I cannot assist with that request.",
		"I cannot read this document.",
		"I cannot comply with this request.",
		"Sorry, but I can't transcribe the document.",
		"Sorry, I can't view this page.",
		"I cannot transcribe the provided image.",
		"I can't transcribe this attached document; it is unreadable.",
		"Unfortunately, I cannot transcribe the image because it is unclear.",
		"I apologize, but I am unable to read the provided page.",
		"I’m sorry, but I can’t transcribe this document.",
		"No puedo leer este documento.",
		"Lo siento, no puedo ver esta página.",
		"Ich kann dieses Dokument nicht lesen.",
		"Entschuldigung, ich kann diese Seite nicht sehen.",
		"Я не можу прочитати цей документ.",
		"Вибачте, я не можу переглянути цю сторінку.",
	} {
		t.Run(text, func(t *testing.T) {
			if _, err := Validate(transcriptionJSON(1, text), 1, 1); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestValidateRejectsHighConfidenceProviderMetaOutput(t *testing.T) {
	for _, text := range []string{
		"As an AI language model, I cannot view the attached pages.",
		"AS AN AI LANGUAGE MODEL: I CAN'T ACCESS THIS ATTACHMENT!",
		"As an AI assistant, I am unable to transcribe the provided image.",
		"As an AI assistant - I can't view this attachment.",
		"I am an AI model and cannot access the attached document.",
		"I'm an AI model; I can't transcribe this image.",
		"I am an AI assistant, and I cannot view the provided pages.",
		"No transcription was produced.",
		"NO TRANSCRIPTION WAS PROVIDED!",
		"No transcription has been produced for this attachment.",
		"No transcription could be provided.",
	} {
		t.Run(text, func(t *testing.T) {
			_, err := Validate(transcriptionJSON(1, text), 1, 1)
			assertSafeProviderError(t, err, []byte(text))
		})
	}
}

func TestValidateRefusedFieldIsAuthoritative(t *testing.T) {
	raw := []byte(`{"pages":[{"page":1,"text":"ordinary visible page text","refused":true}]}`)
	_, err := Validate(raw, 1, 1)
	assertSafeProviderError(t, err, raw)
}

func TestValidateAllowsSimilarDocumentText(t *testing.T) {
	for _, text := range []string{
		"SUMMARY\nThe committee cannot approve expenditures without a quorum.",
		"The witness stated: I cannot comply with this request. The hearing then adjourned.",
		"I'm sorry, but I can't transcribe this document was the exact message displayed by the machine, according to the report.",
		"As an AI language model, I cannot view the attached pages is an example sentence in this textbook chapter.",
		"I cannot transcribe the provided image unless the archive grants access, the curator wrote in the incident log.",
		"Sorry, but I can't transcribe the document was printed in red beneath the scanner's error code.",
		"Unfortunately, I cannot transcribe the image because it is unclear was her summary of the software's response.",
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
	f.Add([]byte(`{"pages":[]} validate-segment-alpha-7c91 validate-segment-beta-3d42`), 1, 1)
	f.Add([]byte{0xff, 'v', 'a', 'l', 'i', 'd', 'a', 't', 'e', '-', 's', 'e', 'g', 'm', 'e', 'n', 't', '-', 'g', 'a', 'm', 'm', 'a', '-', '9', 'f', '2', ' ', 'v', 'a', 'l', 'i', 'd', 'a', 't', 'e', '-', 's', 'e', 'g', 'm', 'e', 'n', 't', '-', 'd', 'e', 'l', 't', 'a', '-', '6', 'b', '1'}, 1, 1)
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
	f.Add(0, "join-segment-alpha-CANARY-4e82 join-segment-beta-19d7")
	f.Add(1, "CANARY-one")
	f.Add(2, "two\nlines")
	f.Fuzz(func(t *testing.T, page int, text string) {
		if len(text) > maxPageTextBytes || !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
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

func assertSafeProviderError(t *testing.T, err error, raw []byte) {
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
	if errorDisclosesInput(err, raw) {
		t.Error("error formatting or unwrap chain disclosed input")
	}
}

func errorDisclosesInput(err error, raw []byte) bool {
	const minDisclosureBytes = 8
	candidates := disclosureCandidates(raw, minDisclosureBytes)
	if len(candidates) == 0 {
		return false
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		for _, formatted := range []string{
			fmt.Sprintf("%s", current),
			fmt.Sprintf("%v", current),
			fmt.Sprintf("%+v", current),
			fmt.Sprintf("%#v", current),
		} {
			for _, candidate := range candidates {
				if strings.Contains(formatted, candidate) {
					return true
				}
			}
		}
	}
	return false
}

func disclosureCandidates(raw []byte, minBytes int) []string {
	if len(raw) < minBytes {
		return nil
	}
	var candidates []string
	if markerBytes(raw) && distinctiveMarker(raw) {
		candidates = append(candidates, string(raw))
	}
	for start := 0; start < len(raw); {
		for start < len(raw) && !isMarkerByte(raw[start]) {
			start++
		}
		end := start
		for end < len(raw) && isMarkerByte(raw[end]) {
			end++
		}
		if end-start >= minBytes && end-start < len(raw) && distinctiveMarker(raw[start:end]) {
			candidates = append(candidates, string(raw[start:end]))
		}
		start = end + 1
	}
	return candidates
}

func markerBytes(value []byte) bool {
	for _, character := range value {
		if !isMarkerByte(character) {
			return false
		}
	}
	return true
}

func distinctiveMarker(value []byte) bool {
	for _, character := range value {
		if character >= '0' && character <= '9' || character == '-' || character == '_' {
			return true
		}
	}
	return false
}

func isMarkerByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '-' || value == '_'
}

func TestErrorDisclosesInputChecksFormattingAndUnwrapChain(t *testing.T) {
	const (
		raw      = "prefix unique-segment-alpha-83f5 middle unique-segment-beta-27c1 suffix"
		segment  = "unique-segment-beta-27c1"
		complete = "unique-complete-canary-6a91"
	)
	tests := []struct {
		name string
		err  error
		raw  string
		want bool
	}{
		{name: "safe", err: saferr.New(saferr.CategoryProvider, invalidTranscriptionMessage), raw: raw, want: false},
		{name: "public formatting", err: errors.New("failed: " + raw), raw: raw, want: true},
		{name: "private unwrap cause", err: saferr.Wrap(saferr.CategoryProvider, invalidTranscriptionMessage, errors.New(raw)), raw: raw, want: true},
		{name: "complete unique canary", err: errors.New("failed: " + complete), raw: complete, want: true},
		{name: "raw substring only", err: errors.New("failed: " + segment), raw: raw, want: true},
		{name: "wrapped raw substring only", err: saferr.Wrap(saferr.CategoryProvider, invalidTranscriptionMessage, errors.New(segment)), raw: raw, want: true},
		{name: "too short to be meaningful", err: errors.New("x"), raw: "x", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := errorDisclosesInput(test.err, []byte(test.raw)); got != test.want {
				t.Errorf("errorDisclosesInput() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestErrorDisclosesInputIgnoresCommonValidationWords(t *testing.T) {
	err := saferr.New(saferr.CategoryProvider, invalidTranscriptionMessage)
	for _, raw := range []string{
		"provider",
		"transcription",
		"AI transcription output is invalid",
		"invalid transcription",
		"pages",
		`{"pages":[{"page":1,"text":"invalid transcription","refused":false}]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if errorDisclosesInput(err, []byte(raw)) {
				t.Error("common validation input counted as disclosure")
			}
		})
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

func caseVariants(value string) []string {
	variants := make([]string, 0, 1<<len(value)-1)
	for mask := 0; mask < 1<<len(value); mask++ {
		variant := []byte(value)
		for index := range variant {
			if mask&(1<<index) != 0 {
				variant[index] -= 'a' - 'A'
			}
		}
		if string(variant) != value {
			variants = append(variants, string(variant))
		}
	}
	return variants
}

func escapedKeyVariants(key string) []string {
	variants := make([]string, 0, len(key)*2)
	seen := make(map[string]struct{})
	for index := range len(key) {
		for _, escape := range []string{fmt.Sprintf(`\u%04x`, key[index]), fmt.Sprintf(`\u%04X`, key[index])} {
			variant := key[:index] + escape + key[index+1:]
			if _, exists := seen[variant]; !exists {
				seen[variant] = struct{}{}
				variants = append(variants, variant)
			}
		}
	}
	return variants
}

func rawWithKeyAtCorrectLevel(key, escaped string) string {
	if key == "pages" {
		return fmt.Sprintf(`{"%s":[{"page":1,"text":"one","refused":false}]}`, escaped)
	}
	switch key {
	case "page":
		return fmt.Sprintf(`{"pages":[{"%s":1,"text":"one","refused":false}]}`, escaped)
	case "text":
		return fmt.Sprintf(`{"pages":[{"page":1,"%s":"one","refused":false}]}`, escaped)
	default:
		return fmt.Sprintf(`{"pages":[{"page":1,"text":"one","%s":false}]}`, escaped)
	}
}

func rawWithKeyAtWrongLevel(key, escaped string) string {
	if key == "pages" {
		return fmt.Sprintf(`{"pages":[{"%s":[],"page":1,"text":"one","refused":false}]}`, escaped)
	}
	return fmt.Sprintf(`{"pages":[{"page":1,"text":"one","refused":false}],"%s":null}`, escaped)
}

func rawWithEscapedAndLiteralKeys(key, escaped string, escapedFirst bool) string {
	first, second := key, escaped
	if escapedFirst {
		first, second = escaped, key
	}
	if key == "pages" {
		return fmt.Sprintf(`{"%s":[],"%s":[{"page":1,"text":"one","refused":false}]}`, first, second)
	}
	value := map[string]string{"page": "1", "text": `"one"`, "refused": "false"}[key]
	return fmt.Sprintf(`{"pages":[{"%s":%s,"%s":%s}]}`, first, value, second, value)
}
