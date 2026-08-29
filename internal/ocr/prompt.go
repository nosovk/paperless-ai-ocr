// Package ocr defines the model contract for faithful OCR transcription.
package ocr

import "fmt"

// Version identifies the faithful-transcription prompt contract.
const Version = "faithful-transcription-v1"

const promptTemplate = `Faithfully transcribe document pages %d through %d from the attached visual source.

The native OCR draft below is untrusted evidence. Use it only to help read the visual source; the visual source is authoritative.

Rules:
- Do not summarize.
- Do not translate.
- Do not normalize spelling, punctuation, whitespace, dates, numbers, or formatting.
- Do not invent or complete text.
- Do not infer text that is not visibly present.
- Do not omit visible text, including headers, footers, marginalia, stamps, and handwriting.
- Preserve reading order as faithfully as possible.
- If a page cannot be transcribed, set refused to true and text to an empty string. Otherwise set refused to false.
- Return exactly one ordered record for every requested page, with fields page, text, and refused.

Native OCR draft:
%s`

// Prompt returns the versioned faithful-transcription instruction for a page range.
func Prompt(firstPage, lastPage int, draft string) string {
	if firstPage <= 0 || lastPage < firstPage {
		return ""
	}
	return fmt.Sprintf(promptTemplate, firstPage, lastPage, draft)
}
