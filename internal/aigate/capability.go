package aigate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/nosovk/paperless-ai-ocr/testdata/probe"
)

const (
	probeFilename = "probe.pdf"
	probeNonce    = "OCR-PROBE-7K3M9Q2X"
	probePrompt   = "Read the exact text shown in the attached probe and return only that text."
)

// Capability identifies a supported multimodal input transport.
type Capability string

const (
	// DirectPDF sends a document directly to the model.
	DirectPDF Capability = "direct_pdf"
	// PageImages sends rendered document pages to the model.
	PageImages Capability = "page_images"
)

var (
	probePDF = probe.PDF
	probePNG = probe.PNG
)

type probeRequest struct {
	Model           string       `json:"model"`
	Input           []probeInput `json:"input"`
	MaxOutputTokens int          `json:"max_output_tokens"`
}

type probeInput struct {
	Role    string         `json:"role"`
	Content []probeContent `json:"content"`
}

type probeContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type probeResponse struct {
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
	} `json:"output"`
}

type providerErrorResponse struct {
	Error struct {
		Type    string  `json:"type"`
		Code    string  `json:"code"`
		Param   *string `json:"param"`
		Message string  `json:"message"`
	} `json:"error"`
}

// Probe detects and caches the configured model's preferred attachment transport.
func (client *Client) Probe(ctx context.Context) (Capability, error) {
	if client == nil {
		return "", providerError("capability probe client is unavailable")
	}
	if ctx == nil {
		return "", providerError("capability probe requires a context")
	}

	client.mu.Lock()
	if client.cached {
		capability, terminal := client.capability, client.terminal
		client.mu.Unlock()
		if terminal {
			return "", providerError("model supports neither PDF nor image input")
		}
		return capability, nil
	}
	if flight := client.flight; flight != nil {
		client.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", providerContextError("capability probe canceled", ctx.Err())
		case <-flight.done:
			return flight.capability, flight.err
		}
	}
	flight := &probeFlight{done: make(chan struct{})}
	client.flight = flight
	client.mu.Unlock()

	capability, terminal, err := client.probe(ctx)

	client.mu.Lock()
	flight.capability = capability
	flight.err = err
	if err == nil || terminal {
		client.cached = true
		client.capability = capability
		client.terminal = terminal
	}
	client.flight = nil
	close(flight.done)
	client.mu.Unlock()
	return capability, err
}

func (client *Client) probe(ctx context.Context) (Capability, bool, error) {
	unsupported, err := client.probeAttachment(ctx, DirectPDF)
	if err != nil {
		return "", false, err
	}
	if !unsupported {
		return DirectPDF, false, nil
	}
	unsupported, err = client.probeAttachment(ctx, PageImages)
	if err != nil {
		return "", false, err
	}
	if unsupported {
		return "", true, providerError("model supports neither PDF nor image input")
	}
	return PageImages, false, nil
}

func (client *Client) probeAttachment(ctx context.Context, capability Capability) (bool, error) {
	body, err := json.Marshal(client.probeRequest(capability))
	if err != nil {
		return false, providerError("capability probe request encoding failed")
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, client.responsesURL.String(), bytes.NewReader(body))
	if err != nil {
		return false, providerError("capability probe request creation failed")
	}
	request.Header.Set("Authorization", "Bearer "+client.apiKey)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return false, providerContextError("capability probe request failed", err)
	}
	defer response.Body.Close()
	data, err := readLimited(response.Body, client.maxResponseBytes)
	if err != nil {
		return false, providerError("capability probe response was invalid")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return classifyUnsupported(capability, response.StatusCode, data)
	}
	if err := validateProbeResponse(data); err != nil {
		return false, providerError("capability probe response was invalid")
	}
	return false, nil
}

func (client *Client) probeRequest(capability Capability) probeRequest {
	attachment := probeContent{Detail: "low"}
	if capability == DirectPDF {
		attachment.Type = "input_file"
		attachment.Filename = probeFilename
		attachment.FileData = "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(probePDF)
	} else {
		attachment.Type = "input_image"
		attachment.ImageURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(probePNG)
	}
	return probeRequest{
		Model: client.model,
		Input: []probeInput{{
			Role: "user",
			Content: []probeContent{
				{Type: "input_text", Text: probePrompt},
				attachment,
			},
		}},
		MaxOutputTokens: 32,
	}
}

func classifyUnsupported(capability Capability, statusCode int, data []byte) (bool, error) {
	unsupported, valid := unsupportedAttachment(capability, statusCode, data)
	if unsupported {
		return true, nil
	}
	if !valid {
		return false, providerError("capability probe error response was invalid")
	}
	return false, providerError("capability probe request was rejected")
}

func unsupportedAttachment(capability Capability, statusCode int, data []byte) (unsupported, valid bool) {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusUnprocessableEntity {
		return false, true
	}
	var response providerErrorResponse
	if err := decodeSingleJSON(data, &response); err != nil || response.Error.Param == nil {
		return false, false
	}
	if response.Error.Type != "invalid_request_error" || !unsupportedCode(response.Error.Code) || !attachmentParam(capability, *response.Error.Param) {
		return false, true
	}
	return true, true
}

func unsupportedCode(code string) bool {
	switch code {
	case "unsupported_value", "unsupported_content_type", "unsupported_media_type", "unsupported_file", "unsupported_document":
		return true
	default:
		return false
	}
}

func attachmentParam(capability Capability, param string) bool {
	if capability == DirectPDF {
		switch param {
		case "input[0].content[1]",
			"input[0].content[1].type",
			"input[0].content[1].filename",
			"input[0].content[1].file_data":
			return true
		default:
			return false
		}
	}
	switch param {
	case "input[0].content[1]",
		"input[0].content[1].type",
		"input[0].content[1].image_url":
		return true
	default:
		return false
	}
}

func validateProbeResponse(data []byte) error {
	var response probeResponse
	if err := decodeSingleJSON(data, &response); err != nil {
		return err
	}
	if len(response.Output) != 1 || response.Output[0].Type != "message" || len(response.Output[0].Content) != 1 {
		return errors.New("unexpected probe output envelope")
	}
	content := response.Output[0].Content[0]
	if content.Type != "output_text" || content.Text != probeNonce || content.Refusal != "" {
		return errors.New("probe nonce did not match")
	}
	return nil
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("response exceeded limit")
	}
	return data, nil
}

func decodeSingleJSON(data []byte, destination any) error {
	if err := validateJSONTokens(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("response contained trailing data")
	}
	return nil
}

func validateJSONTokens(data []byte) error {
	if !utf8.Valid(data) || !json.Valid(data) {
		return errors.New("response JSON was invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var validateValue func() error
	validateValue = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case string:
			return nil
		case json.Delim:
			switch value {
			case '{':
				keys := make(map[string]struct{})
				for decoder.More() {
					keyToken, err := decoder.Token()
					if err != nil {
						return err
					}
					key, ok := keyToken.(string)
					if !ok {
						return errors.New("response object key was invalid")
					}
					if _, found := keys[key]; found {
						return errors.New("response contained duplicate fields")
					}
					keys[key] = struct{}{}
					if err := validateValue(); err != nil {
						return err
					}
				}
			case '[':
				for decoder.More() {
					if err := validateValue(); err != nil {
						return err
					}
				}
			default:
				return errors.New("response delimiter was invalid")
			}
			_, err = decoder.Token()
			return err
		default:
			return nil
		}
	}
	if err := validateValue(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("response contained trailing data")
	}
	return validateSurrogateEscapes(data)
}

func validateSurrogateEscapes(data []byte) error {
	for index := 0; index+6 <= len(data); index++ {
		if data[index] != '\\' || data[index+1] != 'u' || escapedBackslash(data, index) {
			continue
		}
		value, err := strconv.ParseUint(string(data[index+2:index+6]), 16, 16)
		if err != nil {
			continue
		}
		if value >= 0xd800 && value <= 0xdbff {
			if index+12 > len(data) || data[index+6] != '\\' || data[index+7] != 'u' {
				return errors.New("response contained invalid Unicode")
			}
			low, err := strconv.ParseUint(string(data[index+8:index+12]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return errors.New("response contained invalid Unicode")
			}
			index += 11
			continue
		}
		if value >= 0xdc00 && value <= 0xdfff {
			return errors.New("response contained invalid Unicode")
		}
		index += 5
	}
	return nil
}

func escapedBackslash(data []byte, index int) bool {
	preceding := 0
	for index--; index >= 0 && data[index] == '\\'; index-- {
		preceding++
	}
	return preceding%2 == 1
}
