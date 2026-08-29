package ocr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

const (
	maxRawBatchBytes      = 64 << 10
	maxPageTextBytes      = 32 << 10
	maxBatchTextBytes     = 60 << 10
	maxJoinedContentBytes = 16 << 20

	invalidTranscriptionMessage = "AI transcription output is invalid"
	invalidJoinMessage          = "validated transcription batches are invalid"
)

// Page is one validated transcription page.
type Page struct {
	number int
	text   string
}

// Number returns the one-based document page number.
func (page Page) Number() int {
	return page.number
}

// Text returns the faithful page transcription without trimming or newline normalization.
func (page Page) Text() string {
	return page.text
}

// Batch is an immutable validated inclusive range of transcription pages.
type Batch struct {
	firstPage int
	lastPage  int
	pages     []Page
}

// FirstPage returns the first page in the validated batch.
func (batch Batch) FirstPage() int {
	return batch.firstPage
}

// LastPage returns the last page in the validated batch.
func (batch Batch) LastPage() int {
	return batch.lastPage
}

// Pages returns a copy of the ordered validated pages.
func (batch Batch) Pages() []Page {
	return append([]Page(nil), batch.pages...)
}

type rawBatch struct {
	Pages *[]rawPage `json:"pages"`
}

type rawPage struct {
	Page    *int    `json:"page"`
	Text    *string `json:"text"`
	Refused *bool   `json:"refused"`
}

// Validate strictly validates one raw model batch for an exact inclusive range.
func Validate(raw []byte, firstPage, lastPage int) (Batch, error) {
	if !validRange(firstPage, lastPage) || len(raw) == 0 || len(raw) > maxRawBatchBytes || !utf8.Valid(raw) {
		return Batch{}, invalidTranscriptionError()
	}
	if !validJSONUnicodeEscapes(raw) {
		return Batch{}, invalidTranscriptionError()
	}
	if !literalObjectKeys(raw) {
		return Batch{}, invalidTranscriptionError()
	}
	if err := rejectDuplicateFields(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return Batch{}, invalidTranscriptionError()
	}

	var decoded rawBatch
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return Batch{}, invalidTranscriptionError()
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Batch{}, invalidTranscriptionError()
	}
	if decoded.Pages == nil {
		return Batch{}, invalidTranscriptionError()
	}

	expectedCount := lastPage - firstPage + 1
	if len(*decoded.Pages) != expectedCount {
		return Batch{}, invalidTranscriptionError()
	}

	pages := make([]Page, expectedCount)
	totalTextBytes := 0
	for index, decodedPage := range *decoded.Pages {
		if decodedPage.Page == nil || decodedPage.Text == nil || decodedPage.Refused == nil || *decodedPage.Refused {
			return Batch{}, invalidTranscriptionError()
		}
		expectedPage := firstPage + index
		text := *decodedPage.Text
		if *decodedPage.Page != expectedPage || !utf8.ValidString(text) || strings.TrimSpace(text) == "" || len(text) > maxPageTextBytes || suspiciousNonTranscription(text) {
			return Batch{}, invalidTranscriptionError()
		}
		if len(text) > maxBatchTextBytes-totalTextBytes {
			return Batch{}, invalidTranscriptionError()
		}
		totalTextBytes += len(text)
		pages[index] = Page{number: expectedPage, text: text}
	}

	return Batch{firstPage: firstPage, lastPage: lastPage, pages: pages}, nil
}

func literalObjectKeys(raw []byte) bool {
	for index := 0; index < len(raw); index++ {
		if raw[index] != '"' {
			continue
		}
		hasEscape := false
		index++
		for index < len(raw) && raw[index] != '"' {
			if raw[index] == '\\' {
				hasEscape = true
				index++
				if index >= len(raw) {
					return false
				}
			}
			index++
		}
		if index >= len(raw) {
			return false
		}
		next := index + 1
		for next < len(raw) && isJSONWhitespace(raw[next]) {
			next++
		}
		if next < len(raw) && raw[next] == ':' && hasEscape {
			return false
		}
	}
	return true
}

func isJSONWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r'
}

func validJSONUnicodeEscapes(raw []byte) bool {
	for index := 0; index < len(raw); index++ {
		if raw[index] != '"' {
			continue
		}
		index++
		for index < len(raw) && raw[index] != '"' {
			if raw[index] != '\\' {
				index++
				continue
			}
			if index+1 >= len(raw) {
				return false
			}
			if raw[index+1] != 'u' {
				index += 2
				continue
			}
			codeUnit, ok := parseHexCodeUnit(raw, index+2)
			if !ok {
				return false
			}
			index += 6
			switch {
			case codeUnit >= 0xd800 && codeUnit <= 0xdbff:
				if index+1 >= len(raw) || raw[index] != '\\' || raw[index+1] != 'u' {
					return false
				}
				low, ok := parseHexCodeUnit(raw, index+2)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return false
				}
				index += 6
			case codeUnit >= 0xdc00 && codeUnit <= 0xdfff:
				return false
			}
		}
		if index >= len(raw) {
			return false
		}
	}
	return true
}

func parseHexCodeUnit(raw []byte, start int) (uint16, bool) {
	if start > len(raw)-4 {
		return 0, false
	}
	var value uint16
	for _, digit := range raw[start : start+4] {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value |= uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value |= uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value |= uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

// Join assembles validated batches into deterministic Paperless full-text content.
// Each page starts with an ASCII numbered header separated from adjacent content by
// at least one blank line; page transcription bytes are otherwise unchanged.
func Join(batches []Batch) (string, error) {
	if len(batches) == 0 {
		return "", invalidJoinError()
	}

	totalBytes := 0
	expectedPage := 1
	previousText := ""
	for _, batch := range batches {
		if !validBatch(batch, expectedPage) {
			return "", invalidJoinError()
		}
		for _, page := range batch.pages {
			separatorBytes := len(pageSeparator(page))
			if expectedPage != 1 {
				separatorBytes += len(blankLinePadding(previousText))
			}
			if separatorBytes > maxJoinedContentBytes-totalBytes {
				return "", invalidJoinError()
			}
			totalBytes += separatorBytes
			if len(page.text) > maxJoinedContentBytes-totalBytes {
				return "", invalidJoinError()
			}
			totalBytes += len(page.text)
			previousText = page.text
			expectedPage++
		}
	}

	var joined strings.Builder
	joined.Grow(totalBytes)
	expectedPage = 1
	previousText = ""
	for _, batch := range batches {
		for _, page := range batch.pages {
			if expectedPage != 1 {
				joined.WriteString(blankLinePadding(previousText))
			}
			joined.WriteString(pageSeparator(page))
			joined.WriteString(page.text)
			previousText = page.text
			expectedPage++
		}
	}
	return joined.String(), nil
}

func rejectDuplicateFields(decoder *json.Decoder) error {
	if err := scanTranscriptionObject(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}

func scanTranscriptionObject(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return fmt.Errorf("invalid transcription object")
	}
	seenPages := false
	for decoder.More() {
		key, err := nextObjectKey(decoder)
		if err != nil || key != "pages" || seenPages {
			return fmt.Errorf("invalid transcription field")
		}
		seenPages = true
		if err := scanPagesArray(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func scanPagesArray(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return fmt.Errorf("invalid pages array")
	}
	for decoder.More() {
		if err := scanPageObject(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func scanPageObject(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return fmt.Errorf("invalid page object")
	}
	seen := make(map[string]struct{}, 3)
	for decoder.More() {
		key, err := nextObjectKey(decoder)
		if err != nil || !isPageField(key) {
			return fmt.Errorf("invalid page field")
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate page field")
		}
		seen[key] = struct{}{}
		if err := scanJSONValue(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func nextObjectKey(decoder *json.Decoder) (string, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", err
	}
	key, ok := token.(string)
	if !ok {
		return "", fmt.Errorf("invalid object key")
	}
	return key, nil
}

func isPageField(key string) bool {
	return key == "page" || key == "text" || key == "refused"
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return fmt.Errorf("unexpected JSON delimiter")
	}
	for decoder.More() {
		if delimiter == '{' {
			if _, err := nextObjectKey(decoder); err != nil {
				return err
			}
		}
		if err := scanJSONValue(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func validRange(firstPage, lastPage int) bool {
	return firstPage > 0 && lastPage >= firstPage && lastPage-firstPage < maxRawBatchBytes
}

func validBatch(batch Batch, expectedPage int) bool {
	if !validRange(batch.firstPage, batch.lastPage) || batch.firstPage != expectedPage || len(batch.pages) != batch.lastPage-batch.firstPage+1 {
		return false
	}
	totalTextBytes := 0
	for index, page := range batch.pages {
		if page.number != batch.firstPage+index || !utf8.ValidString(page.text) || strings.TrimSpace(page.text) == "" || len(page.text) > maxPageTextBytes || suspiciousNonTranscription(page.text) {
			return false
		}
		if len(page.text) > maxBatchTextBytes-totalTextBytes {
			return false
		}
		totalTextBytes += len(page.text)
	}
	return true
}

func suspiciousNonTranscription(text string) bool {
	words := normalizedWords(text)
	if len(words) == 0 || len(words) > 16 {
		return false
	}
	for _, prefix := range [][]string{
		{"i'm", "sorry"},
		{"i", "am", "sorry"},
		{"sorry"},
		{"unfortunately", "i"},
		{"i", "apologize"},
		{"i", "cannot"},
		{"i", "can't"},
		{"i", "am", "unable"},
		{"as", "an", "ai", "language", "model"},
	} {
		if hasWordPrefix(words, prefix) && refusalTail(words[len(prefix):]) {
			return true
		}
	}
	return false
}

func normalizedWords(text string) []string {
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(text)), "’", "'")
	return strings.FieldsFunc(normalized, func(character rune) bool {
		return !unicode.IsLetter(character) && character != '\''
	})
}

func hasWordPrefix(words, prefix []string) bool {
	if len(words) < len(prefix) {
		return false
	}
	for index := range prefix {
		if words[index] != prefix[index] {
			return false
		}
	}
	return true
}

func refusalTail(words []string) bool {
	hasAction := false
	for _, word := range words {
		switch word {
		case "cannot", "can't", "unable", "assist", "comply", "read", "transcribe", "view":
			hasAction = true
		case "a", "ai", "am", "an", "and", "apologize", "as", "attached", "because", "but", "document", "image", "is", "it", "language", "model", "of", "page", "pages", "provided", "request", "sorry", "that", "the", "this", "to", "unavailable", "unclear", "unreadable", "with", "i", "i'm", "unfortunately":
		default:
			return false
		}
	}
	return hasAction
}

func pageSeparator(page Page) string {
	return fmt.Sprintf("----- PAGE %d; BYTES %d -----\n\n", page.number, len(page.text))
}

func blankLinePadding(previousText string) string {
	if strings.HasSuffix(previousText, "\n\n") {
		return ""
	}
	if strings.HasSuffix(previousText, "\n") {
		return "\n"
	}
	return "\n\n"
}

func invalidTranscriptionError() error {
	return saferr.New(saferr.CategoryProvider, invalidTranscriptionMessage)
}

func invalidJoinError() error {
	return saferr.New(saferr.CategoryProvider, invalidJoinMessage)
}
