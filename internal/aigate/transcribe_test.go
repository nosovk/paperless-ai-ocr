package aigate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/ocr"
	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

var _ Transcriber = (*Client)(nil)

func TestTranscribeDirectPDFRequest(t *testing.T) {
	const draft = "CANARY native OCR draft"
	pdf := []byte("%PDF-CANARY-payload")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeTranscriptionRequest(t, request)
		assertTranscriptionEnvelope(t, request, body, "CANARY-model", 4, 6, draft)
		if len(body.Input[0].Content) != 2 {
			t.Fatalf("content count = %d, want 2", len(body.Input[0].Content))
		}
		attachment := body.Input[0].Content[1]
		if attachment.Type != "input_file" || attachment.Filename != "document.pdf" || attachment.FileData != "data:application/pdf;base64,"+base64.StdEncoding.EncodeToString(pdf) {
			t.Errorf("PDF attachment = %#v", attachment)
		}
		if attachment.ImageURL != "" || attachment.Detail != "" || strings.Contains(attachment.Filename, draft) {
			t.Errorf("PDF attachment contains unexpected fields: %#v", attachment)
		}
		writeTranscriptionSuccess(t, writer, `{"pages":[{"page":4,"text":"four","refused":false}]}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/v1", "CANARY-api-key", "CANARY-model", ClientOptions{})
	raw, err := client.Transcribe(context.Background(), Transcription{
		Capability: DirectPDF, FirstPage: 4, LastPage: 6, OCRDraft: draft, PDF: pdf,
	})
	if err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if got, want := string(raw), `{"pages":[{"page":4,"text":"four","refused":false}]}`; got != want {
		t.Errorf("Transcribe() = %q, want %q", got, want)
	}
}

func TestTranscribePageImagesRequestPreservesOrderAndRange(t *testing.T) {
	images := [][]byte{{0x89, 'P', 'N', 'G', 1}, {0x89, 'P', 'N', 'G', 2}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := decodeTranscriptionRequest(t, request)
		assertTranscriptionEnvelope(t, request, body, "model", 12, 13, "draft")
		if len(body.Input[0].Content) != 3 {
			t.Fatalf("content count = %d, want 3", len(body.Input[0].Content))
		}
		for index, image := range images {
			attachment := body.Input[0].Content[index+1]
			wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(image)
			if attachment.Type != "input_image" || attachment.ImageURL != wantURL || attachment.Detail != "low" {
				t.Errorf("image %d = %#v", index, attachment)
			}
			if attachment.FileData != "" || attachment.Filename != "" {
				t.Errorf("image %d contains file fields: %#v", index, attachment)
			}
		}
		writeTranscriptionSuccess(t, writer, `{"pages":[]}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
	if _, err := client.Transcribe(context.Background(), Transcription{
		Capability: PageImages, FirstPage: 12, LastPage: 13, OCRDraft: "draft", Images: images,
	}); err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
}

func TestTranscribeRejectsInvalidInputBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
	large := make([]byte, maxAttachmentBytes+1)
	tooManyImages := make([][]byte, maxPagesPerRequest+1)
	for index := range tooManyImages {
		tooManyImages[index] = []byte{1}
	}

	tests := []struct {
		name   string
		client *Client
		ctx    context.Context
		input  Transcription
	}{
		{name: "nil receiver", ctx: context.Background(), input: Transcription{Capability: DirectPDF, FirstPage: 1, LastPage: 1, PDF: []byte{1}}},
		{name: "nil context", client: client, input: Transcription{Capability: DirectPDF, FirstPage: 1, LastPage: 1, PDF: []byte{1}}},
		{name: "unknown capability", client: client, ctx: context.Background(), input: Transcription{Capability: Capability("unknown"), FirstPage: 1, LastPage: 1, PDF: []byte{1}}},
		{name: "zero first page", client: client, ctx: context.Background(), input: Transcription{Capability: DirectPDF, LastPage: 1, PDF: []byte{1}}},
		{name: "reversed range", client: client, ctx: context.Background(), input: Transcription{Capability: DirectPDF, FirstPage: 2, LastPage: 1, PDF: []byte{1}}},
		{name: "too many pages", client: client, ctx: context.Background(), input: Transcription{Capability: DirectPDF, FirstPage: 1, LastPage: maxPagesPerRequest + 1, PDF: []byte{1}}},
		{name: "empty PDF", client: client, ctx: context.Background(), input: Transcription{Capability: DirectPDF, FirstPage: 1, LastPage: 1}},
		{name: "PDF with images", client: client, ctx: context.Background(), input: Transcription{Capability: DirectPDF, FirstPage: 1, LastPage: 1, PDF: []byte{1}, Images: [][]byte{{1}}}},
		{name: "large PDF", client: client, ctx: context.Background(), input: Transcription{Capability: DirectPDF, FirstPage: 1, LastPage: 1, PDF: large}},
		{name: "empty images", client: client, ctx: context.Background(), input: Transcription{Capability: PageImages, FirstPage: 1, LastPage: 1}},
		{name: "wrong image count", client: client, ctx: context.Background(), input: Transcription{Capability: PageImages, FirstPage: 1, LastPage: 2, Images: [][]byte{{1}}}},
		{name: "empty image", client: client, ctx: context.Background(), input: Transcription{Capability: PageImages, FirstPage: 1, LastPage: 1, Images: [][]byte{{}}}},
		{name: "too many images", client: client, ctx: context.Background(), input: Transcription{Capability: PageImages, FirstPage: 1, LastPage: maxPagesPerRequest + 1, Images: tooManyImages}},
		{name: "large image", client: client, ctx: context.Background(), input: Transcription{Capability: PageImages, FirstPage: 1, LastPage: 1, Images: [][]byte{large}}},
		{name: "images with PDF", client: client, ctx: context.Background(), input: Transcription{Capability: PageImages, FirstPage: 1, LastPage: 1, PDF: []byte{1}, Images: [][]byte{{1}}}},
		{name: "excessive draft", client: client, ctx: context.Background(), input: Transcription{Capability: DirectPDF, FirstPage: 1, LastPage: 1, OCRDraft: strings.Repeat("x", maxOCRDraftBytes+1), PDF: []byte{1}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.client.Transcribe(test.ctx, test.input)
			if providerCategory(err) != saferr.CategoryProvider {
				t.Fatalf("category = %q, want provider", providerCategory(err))
			}
		})
	}
	if requests.Load() != 0 {
		t.Errorf("network requests = %d, want 0", requests.Load())
	}
}

func TestTranscribeRejectsExcessiveEncodedRequestBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
	images := make([][]byte, maxPagesPerRequest)
	for index := range images {
		images[index] = make([]byte, maxAttachmentBytes)
	}
	_, err := client.Transcribe(context.Background(), Transcription{
		Capability: PageImages, FirstPage: 1, LastPage: maxPagesPerRequest, Images: images,
	})
	if providerCategory(err) != saferr.CategoryProvider {
		t.Fatalf("category = %q, want provider", providerCategory(err))
	}
	if requests.Load() != 0 {
		t.Errorf("network requests = %d, want 0", requests.Load())
	}
}

func TestTranscribeRetryClassification(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		retryAfter string
		wantClass  RetryClass
		wantDelay  time.Duration
		dateDelay  time.Duration
		wantRetry  bool
	}{
		{name: "rate limit delay", status: http.StatusTooManyRequests, retryAfter: "17", wantClass: RetryRateLimit, wantDelay: 17 * time.Second, wantRetry: true},
		{name: "rate limit date", status: http.StatusTooManyRequests, dateDelay: 45 * time.Second, wantClass: RetryRateLimit, wantRetry: true},
		{name: "rate limit invalid delay", status: http.StatusTooManyRequests, retryAfter: "secret-invalid", wantClass: RetryRateLimit, wantRetry: true},
		{name: "server", status: http.StatusBadGateway, retryAfter: "3", wantClass: RetryUnavailable, wantDelay: 3 * time.Second, wantRetry: true},
		{name: "auth", status: http.StatusUnauthorized, retryAfter: "9"},
		{name: "permanent", status: http.StatusBadRequest, retryAfter: "9"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			retryAfter := test.retryAfter
			if test.dateDelay != 0 {
				retryAfter = time.Now().UTC().Add(test.dateDelay).Format(http.TimeFormat)
			}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Retry-After", retryAfter)
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, `{"error":{"message":"CANARY-provider-secret"}}`)
			}))
			defer server.Close()
			client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
			_, err := client.Transcribe(context.Background(), Transcription{Capability: DirectPDF, FirstPage: 1, LastPage: 1, PDF: []byte{1}})
			class, delay, retry := Retry(err)
			if class != test.wantClass || retry != test.wantRetry {
				t.Errorf("Retry() = %q, %v, %t, want %q, %v, %t", class, delay, retry, test.wantClass, test.wantDelay, test.wantRetry)
			}
			if test.dateDelay != 0 && (delay < test.dateDelay-time.Second || delay > test.dateDelay) {
				t.Errorf("Retry() date delay = %v, want within [%v, %v]", delay, test.dateDelay-time.Second, test.dateDelay)
			} else if test.dateDelay == 0 && delay != test.wantDelay {
				t.Errorf("Retry() delay = %v, want %v", delay, test.wantDelay)
			}
			if providerCategory(err) != saferr.CategoryProvider {
				t.Errorf("category = %q, want provider", providerCategory(err))
			}
		})
	}
}

func TestTranscribeDistinguishesAuthenticationFromPermanentFailure(t *testing.T) {
	errorsByStatus := make(map[int]string)
	for _, status := range []int{http.StatusUnauthorized, http.StatusBadRequest} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(status)
			_, _ = io.WriteString(writer, `{"error":{"message":"CANARY-provider-secret"}}`)
		}))
		client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
		_, err := client.Transcribe(context.Background(), Transcription{Capability: DirectPDF, FirstPage: 1, LastPage: 1, PDF: []byte{1}})
		server.Close()
		if providerCategory(err) != saferr.CategoryProvider {
			t.Fatalf("status %d category = %q, want provider", status, providerCategory(err))
		}
		errorsByStatus[status] = err.Error()
	}
	if errorsByStatus[http.StatusUnauthorized] == errorsByStatus[http.StatusBadRequest] {
		t.Errorf("authentication and permanent failures have the same classification %q", errorsByStatus[http.StatusUnauthorized])
	}
}

func TestTranscribeMalformedSuccessIsPermanent(t *testing.T) {
	for name, response := range map[string]string{
		"malformed envelope": `{`,
		"trailing envelope":  `{"output":[]} {}`,
		"malformed output":   `{"output":[{"type":"message","content":[{"type":"output_text","text":"{"}]}]}`,
		"trailing output":    `{"output":[{"type":"message","content":[{"type":"output_text","text":"{} {}"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(writer, response) }))
			defer server.Close()
			client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
			_, err := client.Transcribe(context.Background(), Transcription{Capability: DirectPDF, FirstPage: 1, LastPage: 1, PDF: []byte{1}})
			if _, _, retry := Retry(err); retry {
				t.Error("malformed 2xx response classified as retryable")
			}
			if providerCategory(err) != saferr.CategoryProvider {
				t.Errorf("category = %q, want provider", providerCategory(err))
			}
		})
	}
}

func TestTranscribeBoundsResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, strings.Repeat("x", 129))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{MaxResponseBytes: 128})
	_, err := client.Transcribe(context.Background(), Transcription{Capability: DirectPDF, FirstPage: 1, LastPage: 1, PDF: []byte{1}})
	if providerCategory(err) != saferr.CategoryProvider {
		t.Fatalf("category = %q, want provider", providerCategory(err))
	}
	if _, _, retry := Retry(err); retry {
		t.Error("oversized response classified as retryable")
	}
}

func TestTranscribeRetryableStatusSurvivesOversizedProviderBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "2")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, strings.Repeat("CANARY-provider-secret", 20))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{MaxResponseBytes: 128})
	_, err := client.Transcribe(context.Background(), Transcription{Capability: DirectPDF, FirstPage: 1, LastPage: 1, PDF: []byte{1}})
	class, delay, retry := Retry(err)
	if class != RetryRateLimit || delay != 2*time.Second || !retry {
		t.Errorf("Retry() = %q, %v, %t, want %q, 2s, true", class, delay, retry, RetryRateLimit)
	}
}

func TestTranscribeCancellationAndTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	input := Transcription{Capability: DirectPDF, FirstPage: 1, LastPage: 1, PDF: []byte{1}}

	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{RequestTimeout: 10 * time.Millisecond})
	if _, err := client.Transcribe(context.Background(), input); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v, want context.DeadlineExceeded", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Transcribe(ctx, input); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v, want context.Canceled", err)
	}
}

func TestTranscribeErrorsRedactAllSensitiveValuesAndCauses(t *testing.T) {
	const (
		apiKey   = "CANARY-api-key-secret"
		model    = "CANARY-model-secret"
		draft    = "CANARY-draft-secret"
		filename = "document.pdf"
		message  = "CANARY-provider-message-secret"
	)
	pdf := []byte("CANARY-document-bytes-secret")
	dataURL := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdf)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(writer, `{"error":{"message":%q},"body":%q}`, message, "CANARY-provider-body-secret")
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v1/CANARY-base-url-secret", apiKey, model, ClientOptions{})
	_, err := client.Transcribe(context.Background(), Transcription{Capability: DirectPDF, FirstPage: 1, LastPage: 1, OCRDraft: draft, PDF: pdf})
	if providerCategory(err) != saferr.CategoryProvider {
		t.Fatalf("category = %q, want provider", providerCategory(err))
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		formatted := fmt.Sprintf("%s|%v|%+v|%#v", current, current, current, current)
		for _, secret := range []string{apiKey, model, draft, filename, message, "CANARY-provider-body-secret", "CANARY-base-url-secret", string(pdf), dataURL, base64.StdEncoding.EncodeToString(pdf)} {
			if strings.Contains(formatted, secret) {
				t.Errorf("error chain disclosed %q in %q", secret, formatted)
			}
		}
	}
}

type transcriptionRequestBody struct {
	Model           string                  `json:"model"`
	Input           []responseItem          `json:"input"`
	Text            transcriptionTextFormat `json:"text"`
	MaxOutputTokens int                     `json:"max_output_tokens"`
}

type transcriptionTextFormat struct {
	Format decodedTranscriptionFormat `json:"format"`
}

type decodedTranscriptionFormat struct {
	Type   string         `json:"type"`
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

func decodeTranscriptionRequest(t *testing.T, request *http.Request) transcriptionRequestBody {
	t.Helper()
	if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/v1/responses") {
		t.Errorf("request = %s %s, want POST */v1/responses", request.Method, request.URL.Path)
	}
	if request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Accept") != "application/json" {
		t.Errorf("content negotiation headers = %q/%q", request.Header.Get("Content-Type"), request.Header.Get("Accept"))
	}
	var body transcriptionRequestBody
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return body
}

func assertTranscriptionEnvelope(t *testing.T, request *http.Request, body transcriptionRequestBody, model string, firstPage, lastPage int, draft string) {
	t.Helper()
	if request.Header.Get("Authorization") == "" {
		t.Error("Authorization header is blank")
	}
	if body.Model != model || body.MaxOutputTokens != maxOutputTokens {
		t.Errorf("model/tokens = %q/%d, want %q/%d", body.Model, body.MaxOutputTokens, model, maxOutputTokens)
	}
	if len(body.Input) != 1 || body.Input[0].Role != "user" || len(body.Input[0].Content) < 2 {
		t.Fatalf("input = %#v", body.Input)
	}
	prompt := body.Input[0].Content[0]
	if prompt.Type != "input_text" || prompt.Text != ocr.Prompt(firstPage, lastPage, draft) || !strings.Contains(prompt.Text, draft) {
		t.Errorf("prompt = %#v", prompt)
	}
	format := body.Text.Format
	if format.Type != "json_schema" || format.Name != "page_transcription" || !format.Strict {
		t.Errorf("format = %#v", format)
	}
	assertStrictTranscriptionSchema(t, format.Schema)
}

func assertStrictTranscriptionSchema(t *testing.T, schema map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	const want = `{"additionalProperties":false,"properties":{"pages":{"items":{"additionalProperties":false,"properties":{"page":{"minimum":1,"type":"integer"},"refused":{"type":"boolean"},"text":{"type":"string"}},"required":["page","text","refused"],"type":"object"},"type":"array"}},"required":["pages"],"type":"object"}`
	if string(encoded) != want {
		t.Errorf("schema = %s, want %s", encoded, want)
	}
}

func writeTranscriptionSuccess(t *testing.T, writer http.ResponseWriter, raw string) {
	t.Helper()
	response := struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}{}
	response.Output = append(response.Output, struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}{Type: "message", Content: []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{{Type: "output_text", Text: raw}}})
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
