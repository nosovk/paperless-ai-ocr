package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/nosovk/paperless-ai-ocr/internal/queue"
)

const maxWebhookBodyBytes = 4 << 10

type tokenComparator func([]byte, []byte) int

type webhookHandler struct {
	tokenHash [sha256.Size]byte
	enqueuer  CandidateEnqueuer
}

type webhookPayload struct {
	DocumentID int64 `json:"document_id"`
}

func (h webhookHandler) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if len(request.Header.Values("Authorization")) != 1 ||
		!verifyBearerToken(request.Header.Get("Authorization"), h.tokenHash, subtle.ConstantTimeCompare) {
		response.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}

	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(response, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}

	payload, status := decodeWebhookPayload(response, request)
	if status != 0 {
		http.Error(response, http.StatusText(status), status)
		return
	}
	if _, err := h.enqueuer.EnqueueCandidate(request.Context(), payload.DocumentID, queue.PriorityWebhook); err != nil {
		http.Error(response, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	response.WriteHeader(http.StatusAccepted)
}

func decodeWebhookPayload(response http.ResponseWriter, request *http.Request) (webhookPayload, int) {
	request.Body = http.MaxBytesReader(response, request.Body, maxWebhookBodyBytes)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return webhookPayload{}, decodeErrorStatus(err)
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') || !decoder.More() {
		return webhookPayload{}, http.StatusBadRequest
	}
	key, err := decoder.Token()
	if err != nil || key != "document_id" {
		return webhookPayload{}, http.StatusBadRequest
	}
	var documentID int64
	if err := decoder.Decode(&documentID); err != nil || documentID <= 0 || decoder.More() {
		return webhookPayload{}, http.StatusBadRequest
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return webhookPayload{}, http.StatusBadRequest
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return webhookPayload{}, http.StatusBadRequest
	}
	return webhookPayload{DocumentID: documentID}, 0
}

func decodeErrorStatus(err error) int {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func hashToken(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(token))
}

func verifyBearerToken(authorization string, expected [sha256.Size]byte, compare tokenComparator) bool {
	if len(authorization) < len("Bearer ")+1 || authorization[len("Bearer")] != ' ' ||
		!equalFoldASCII(authorization[:len("Bearer")], "Bearer") {
		return false
	}
	credential := authorization[len("Bearer "):]
	if !validBearerCredential(credential) {
		return false
	}
	presented := hashToken(credential)
	return compare(presented[:], expected[:]) == 1
}

func validBearerCredential(credential string) bool {
	if credential == "" {
		return false
	}
	for index := range len(credential) {
		character := credential[index]
		if character <= 0x20 || character >= 0x7f || character == ',' {
			return false
		}
	}
	return true
}

func equalFoldASCII(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range len(left) {
		leftCharacter := left[index]
		rightCharacter := right[index]
		if leftCharacter >= 'A' && leftCharacter <= 'Z' {
			leftCharacter += 'a' - 'A'
		}
		if rightCharacter >= 'A' && rightCharacter <= 'Z' {
			rightCharacter += 'a' - 'A'
		}
		if leftCharacter != rightCharacter {
			return false
		}
	}
	return true
}
