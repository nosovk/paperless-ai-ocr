// Package server provides the service's HTTP routes.
package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/nosovk/paperless-ai-ocr/internal/queue"
)

// CandidateEnqueuer durably records documents for later job resolution.
type CandidateEnqueuer interface {
	EnqueueCandidate(context.Context, int64, queue.Priority) (bool, error)
}

// New constructs the service HTTP routes.
func New(webhookToken string, enqueuer CandidateEnqueuer) (*http.ServeMux, error) {
	if !validBearerCredential(webhookToken) || enqueuer == nil {
		return nil, errors.New("invalid server configuration")
	}

	mux := http.NewServeMux()
	handler := webhookHandler{
		tokenHash: hashToken(webhookToken),
		enqueuer:  enqueuer,
	}
	mux.HandleFunc("POST /webhooks/paperless", handler.serveHTTP)
	return mux, nil
}
