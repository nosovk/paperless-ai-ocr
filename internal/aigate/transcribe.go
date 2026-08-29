package aigate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/ocr"
)

const (
	transcriptionFilename = "document.pdf"
	maxPagesPerRequest    = 5
	maxAttachmentBytes    = 8 << 20
	maxOCRDraftBytes      = 1 << 20
	maxRequestBytes       = 32 << 20
	maxOutputTokens       = 8192
)

// Transcription describes one explicitly selected multimodal page request.
type Transcription struct {
	Capability Capability
	FirstPage  int
	LastPage   int
	OCRDraft   string
	PDF        []byte
	Images     [][]byte
}

// Transcriber executes explicitly selected multimodal transcription requests.
type Transcriber interface {
	Transcribe(context.Context, Transcription) (json.RawMessage, error)
}

// RetryClass identifies the provider condition that made a request retryable.
type RetryClass string

const (
	// RetryRateLimit indicates provider rate limiting.
	RetryRateLimit RetryClass = "rate_limit"
	// RetryUnavailable indicates a temporary provider-side failure.
	RetryUnavailable RetryClass = "unavailable"
)

type retryError struct {
	class RetryClass
	delay time.Duration
	err   error
}

func (err *retryError) Error() string { return err.err.Error() }

func (err *retryError) Format(state fmt.State, verb rune) {
	fmt.Fprintf(state, "%"+string(verb), err.err)
}

func (err *retryError) Unwrap() error { return err.err }

// Retry reports safe retry metadata without exposing provider response data.
func Retry(err error) (RetryClass, time.Duration, bool) {
	retry, ok := errors.AsType[*retryError](err)
	if !ok {
		return "", 0, false
	}
	return retry.class, retry.delay, true
}

type transcriptionRequest struct {
	Model           string            `json:"model"`
	Input           []transcriptionIn `json:"input"`
	Text            transcriptionText `json:"text"`
	MaxOutputTokens int               `json:"max_output_tokens"`
}

type transcriptionIn struct {
	Role    string                 `json:"role"`
	Content []transcriptionContent `json:"content"`
}

type transcriptionContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type transcriptionText struct {
	Format transcriptionFormat `json:"format"`
}

type transcriptionFormat struct {
	Type   string              `json:"type"`
	Name   string              `json:"name"`
	Strict bool                `json:"strict"`
	Schema transcriptionSchema `json:"schema"`
}

type transcriptionSchema struct {
	Type                 string                           `json:"type"`
	Properties           map[string]transcriptionProperty `json:"properties"`
	Required             []string                         `json:"required"`
	AdditionalProperties bool                             `json:"additionalProperties"`
}

type transcriptionProperty struct {
	Type    string                   `json:"type"`
	Minimum *int                     `json:"minimum,omitempty"`
	Items   *transcriptionPageSchema `json:"items,omitempty"`
}

type transcriptionPageSchema struct {
	Type                 string                           `json:"type"`
	Properties           map[string]transcriptionProperty `json:"properties"`
	Required             []string                         `json:"required"`
	AdditionalProperties bool                             `json:"additionalProperties"`
}

type transcriptionResponse struct {
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
	} `json:"output"`
}

// Transcribe executes one explicitly selected multimodal Responses API request.
func (client *Client) Transcribe(ctx context.Context, input Transcription) (json.RawMessage, error) {
	if client == nil {
		return nil, providerError("transcription client is unavailable")
	}
	if ctx == nil {
		return nil, providerError("transcription requires a context")
	}
	if err := validateTranscription(input); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, providerContextError("transcription canceled", err)
	}

	body, err := json.Marshal(client.transcriptionRequest(input))
	if err != nil || len(body) > maxRequestBytes {
		return nil, providerError("transcription request exceeded limit")
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, client.responsesURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, providerError("transcription request creation failed")
	}
	request.Header.Set("Authorization", "Bearer "+client.apiKey)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, providerContextError("transcription request failed", err)
	}
	defer response.Body.Close()
	data, err := readLimited(response.Body, client.maxResponseBytes)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, client.transcriptionStatusError(response.StatusCode, response.Header.Get("Retry-After"))
	}
	if err != nil {
		return nil, providerError("transcription response was invalid")
	}
	raw, err := transcriptionOutput(data)
	if err != nil {
		return nil, providerError("transcription response was invalid")
	}
	return raw, nil
}

func validateTranscription(input Transcription) error {
	pageCount := int64(input.LastPage) - int64(input.FirstPage) + 1
	if input.FirstPage <= 0 || input.LastPage < input.FirstPage || pageCount > maxPagesPerRequest {
		return providerError("invalid transcription page range")
	}
	if len(input.OCRDraft) > maxOCRDraftBytes {
		return providerError("transcription OCR draft exceeded limit")
	}
	switch input.Capability {
	case DirectPDF:
		if len(input.PDF) == 0 || len(input.PDF) > maxAttachmentBytes || len(input.Images) != 0 {
			return providerError("invalid PDF transcription input")
		}
	case PageImages:
		if len(input.PDF) != 0 || len(input.Images) != int(pageCount) {
			return providerError("invalid image transcription input")
		}
		for _, image := range input.Images {
			if len(image) == 0 || len(image) > maxAttachmentBytes {
				return providerError("invalid image transcription input")
			}
		}
	default:
		return providerError("invalid transcription capability")
	}
	return nil
}

func (client *Client) transcriptionRequest(input Transcription) transcriptionRequest {
	content := []transcriptionContent{{Type: "input_text", Text: ocr.Prompt(input.FirstPage, input.LastPage, input.OCRDraft)}}
	if input.Capability == DirectPDF {
		content = append(content, transcriptionContent{
			Type:     "input_file",
			Filename: transcriptionFilename,
			FileData: dataURL("application/pdf", input.PDF),
		})
	} else {
		for _, image := range input.Images {
			content = append(content, transcriptionContent{
				Type:     "input_image",
				ImageURL: dataURL("image/png", image),
				Detail:   "low",
			})
		}
	}
	return transcriptionRequest{
		Model: client.model,
		Input: []transcriptionIn{{
			Role:    "user",
			Content: content,
		}},
		Text:            transcriptionText{Format: strictTranscriptionFormat()},
		MaxOutputTokens: maxOutputTokens,
	}
}

func dataURL(mediaType string, data []byte) string {
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func strictTranscriptionFormat() transcriptionFormat {
	minimumPage := 1
	return transcriptionFormat{
		Type:   "json_schema",
		Name:   "page_transcription",
		Strict: true,
		Schema: transcriptionSchema{
			Type: "object",
			Properties: map[string]transcriptionProperty{
				"pages": {
					Type: "array",
					Items: &transcriptionPageSchema{
						Type: "object",
						Properties: map[string]transcriptionProperty{
							"page":    {Type: "integer", Minimum: &minimumPage},
							"text":    {Type: "string"},
							"refused": {Type: "boolean"},
						},
						Required:             []string{"page", "text", "refused"},
						AdditionalProperties: false,
					},
				},
			},
			Required:             []string{"pages"},
			AdditionalProperties: false,
		},
	}
}

func transcriptionOutput(data []byte) (json.RawMessage, error) {
	var response transcriptionResponse
	if err := decodeSingleJSON(data, &response); err != nil {
		return nil, err
	}
	if len(response.Output) != 1 || response.Output[0].Type != "message" || len(response.Output[0].Content) != 1 {
		return nil, errors.New("unexpected transcription output envelope")
	}
	content := response.Output[0].Content[0]
	if content.Type != "output_text" || content.Text == "" || content.Refusal != "" {
		return nil, errors.New("unexpected transcription output content")
	}
	raw := json.RawMessage(content.Text)
	var value any
	if err := decodeSingleJSON(raw, &value); err != nil {
		return nil, err
	}
	return bytes.Clone(raw), nil
}

func (client *Client) transcriptionStatusError(statusCode int, retryAfter string) error {
	var class RetryClass
	switch {
	case statusCode == http.StatusTooManyRequests:
		class = RetryRateLimit
	case statusCode >= http.StatusInternalServerError && statusCode < 600:
		class = RetryUnavailable
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return providerError("transcription authentication failed")
	default:
		return providerError("transcription request was rejected")
	}
	return &retryError{
		class: class,
		delay: parseRetryAfter(retryAfter, time.Now()),
		err:   providerError("transcription provider is temporarily unavailable"),
	}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseUint(value, 10, 31); err == nil {
		return time.Duration(seconds) * time.Second
	}
	date, err := http.ParseTime(value)
	if err != nil || !date.After(now) {
		return 0
	}
	return date.Sub(now)
}
