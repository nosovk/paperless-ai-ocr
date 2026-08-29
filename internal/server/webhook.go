package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

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
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var payload webhookPayload
	if err := decoder.Decode(&payload); err != nil {
		return webhookPayload{}, decodeErrorStatus(err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return webhookPayload{}, decodeErrorStatus(err)
	}
	if payload.DocumentID <= 0 {
		return webhookPayload{}, http.StatusBadRequest
	}
	return payload, 0
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
	fields := strings.Fields(authorization)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return false
	}
	presented := hashToken(fields[1])
	return compare(presented[:], expected[:]) == 1
}
