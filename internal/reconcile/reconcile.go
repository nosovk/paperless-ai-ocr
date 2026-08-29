// Package reconcile discovers durable work missed by webhook delivery.
package reconcile

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/paperless"
	"github.com/nosovk/paperless-ai-ocr/internal/queue"
	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

const (
	checkpointKey             = "reconcile.documents.next"
	defaultMaxCandidates      = 100
	defaultMaxArchivePages    = 1
	defaultMaxPagesPerScan    = 10_000
	checkpointVersion         = 1
	checkpointTimestampLayout = "2006-01-02T15:04:05.000000000Z07:00"
	initialCursorIdentifier   = "paperless-documents-initial-page"
)

// Options configures one bounded reconciliation pass.
type Options struct {
	Model                  string
	PromptVersion          string
	MaxCandidatesPerPass   int
	MaxArchivePagesPerPass int
	MaxArchivePagesPerScan int
	Yield                  func(context.Context) error
}

// Report contains non-sensitive reconciliation progress counts.
type Report struct {
	CandidatesResolved  int
	CandidatesDiscarded int
	DocumentsSeen       int
	JobsCreated         int
	PagesProcessed      int
	ScanComplete        bool
}

// Reconciler resolves webhook candidates before scanning archive pages.
type Reconciler struct {
	db                     *sql.DB
	paperless              *paperless.Client
	queue                  *queue.Queue
	model                  string
	promptVersion          string
	maxCandidatesPerPass   int
	maxArchivePagesPerPass int
	maxArchivePagesPerScan int
	yield                  func(context.Context) error
}

// New constructs a bounded reconciler.
func New(db *sql.DB, client *paperless.Client, q *queue.Queue, options Options) (*Reconciler, error) {
	if db == nil || client == nil || q == nil || strings.TrimSpace(options.Model) == "" || strings.TrimSpace(options.PromptVersion) == "" || options.MaxCandidatesPerPass < 0 || options.MaxArchivePagesPerPass < 0 || options.MaxArchivePagesPerScan < 0 {
		return nil, saferr.New(saferr.CategoryValidation, "invalid reconciler configuration")
	}
	if options.MaxCandidatesPerPass == 0 {
		options.MaxCandidatesPerPass = defaultMaxCandidates
	}
	if options.MaxArchivePagesPerPass == 0 {
		options.MaxArchivePagesPerPass = defaultMaxArchivePages
	}
	if options.MaxArchivePagesPerScan == 0 {
		options.MaxArchivePagesPerScan = defaultMaxPagesPerScan
	}
	if options.Yield == nil {
		options.Yield = func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			runtime.Gosched()
			return nil
		}
	}
	return &Reconciler{
		db: db, paperless: client, queue: q, model: options.Model,
		promptVersion:          options.PromptVersion,
		maxCandidatesPerPass:   options.MaxCandidatesPerPass,
		maxArchivePagesPerPass: options.MaxArchivePagesPerPass,
		maxArchivePagesPerScan: options.MaxArchivePagesPerScan,
		yield:                  options.Yield,
	}, nil
}

// RunOnce resolves bounded candidate and archive work, persisting page progress.
func (r *Reconciler) RunOnce(ctx context.Context) (Report, error) {
	var report Report
	for range r.maxCandidatesPerPass {
		candidate, ok, err := r.queue.NextCandidate(ctx)
		if err != nil {
			return report, reconcileError("cannot select candidate", err)
		}
		if !ok {
			break
		}
		if candidate.DocumentID > math.MaxInt {
			return report, saferr.New(saferr.CategoryValidation, "candidate document ID is unsupported")
		}
		document, err := r.paperless.GetDocument(ctx, int(candidate.DocumentID))
		if err != nil {
			var statusErr *paperless.StatusError
			if errors.As(err, &statusErr) && statusErr.StatusCode == 404 {
				if err := r.queue.DiscardCandidate(ctx, candidate.DocumentID); err != nil {
					return report, reconcileError("cannot discard missing candidate", err)
				}
				report.CandidatesDiscarded++
				continue
			}
			return report, reconcileError("cannot resolve candidate", err)
		}
		if strings.TrimSpace(document.Checksum) == "" {
			if err := r.queue.DiscardCandidate(ctx, candidate.DocumentID); err != nil {
				return report, reconcileError("cannot discard invalid candidate", err)
			}
			report.CandidatesDiscarded++
			continue
		}
		_, created, err := r.queue.ResolveCandidate(ctx, candidate.DocumentID, queue.EnqueueInput{
			DocumentID: candidate.DocumentID, SourceChecksum: document.Checksum,
			Priority: candidate.Priority, Model: r.model, PromptVersion: r.promptVersion,
		})
		if err != nil {
			return report, reconcileError("cannot persist resolved candidate", err)
		}
		report.CandidatesResolved++
		if created {
			report.JobsCreated++
		}
	}
	if _, ok, err := r.queue.NextCandidate(ctx); err != nil {
		return report, reconcileError("cannot inspect remaining candidates", err)
	} else if ok {
		return report, nil
	}

	checkpoint, err := r.checkpoint(ctx)
	if err != nil {
		return report, err
	}
	for pageNumber := range r.maxArchivePagesPerPass {
		if checkpoint.Pages >= r.maxArchivePagesPerScan {
			return report, paginationError("archive page limit exceeded")
		}
		currentFingerprint := fingerprintCursor(checkpoint.Next)
		if slicesContains(checkpoint.Visited, currentFingerprint) {
			return report, paginationError("archive pagination loop detected")
		}
		page, err := r.paperless.ListDocumentsPage(ctx, checkpoint.Next)
		if err != nil {
			return report, reconcileError("cannot list archive page", err)
		}
		for _, document := range page.Documents {
			report.DocumentsSeen++
			if strings.TrimSpace(document.Checksum) == "" {
				return report, saferr.New(saferr.CategoryPaperless, "archive document checksum is blank")
			}
			_, created, err := r.queue.EnqueueContext(ctx, queue.EnqueueInput{
				DocumentID: int64(document.ID), SourceChecksum: document.Checksum,
				Priority: queue.PriorityBackfill, Model: r.model, PromptVersion: r.promptVersion,
			})
			if err != nil {
				return report, reconcileError("cannot persist archive document", err)
			}
			if created {
				report.JobsCreated++
			}
		}
		checkpoint.Visited = append(checkpoint.Visited, currentFingerprint)
		checkpoint.Pages++
		checkpoint.Next = page.Next
		report.PagesProcessed++
		if checkpoint.Next == "" {
			if err := r.clearCheckpoint(ctx); err != nil {
				return report, err
			}
			report.ScanComplete = true
			return report, nil
		}
		blockedByLoop := slicesContains(checkpoint.Visited, fingerprintCursor(checkpoint.Next))
		blockedByLimit := checkpoint.Pages >= r.maxArchivePagesPerScan
		if err := r.setCheckpoint(ctx, checkpoint); err != nil {
			return report, err
		}
		if blockedByLoop {
			return report, paginationError("archive pagination loop detected")
		}
		if blockedByLimit {
			return report, paginationError("archive page limit exceeded")
		}
		if pageNumber+1 < r.maxArchivePagesPerPass {
			if err := r.yield(ctx); err != nil {
				return report, reconcileError("archive page yield failed", err)
			}
		}
	}
	return report, nil
}

type archiveCheckpoint struct {
	Version int      `json:"version"`
	Next    string   `json:"next"`
	Visited []string `json:"visited"`
	Pages   int      `json:"pages"`
}

func (r *Reconciler) checkpoint(ctx context.Context) (archiveCheckpoint, error) {
	var value string
	err := r.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", checkpointKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return archiveCheckpoint{Version: checkpointVersion, Visited: []string{}}, nil
	}
	if err != nil {
		return archiveCheckpoint{}, reconcileError("cannot load archive checkpoint", err)
	}
	var checkpoint archiveCheckpoint
	if err := json.Unmarshal([]byte(value), &checkpoint); err != nil || checkpoint.Version != checkpointVersion || checkpoint.Next == "" || checkpoint.Pages <= 0 || len(checkpoint.Visited) != checkpoint.Pages {
		return archiveCheckpoint{}, saferr.New(saferr.CategoryInternal, "archive checkpoint is invalid")
	}
	for _, fingerprint := range checkpoint.Visited {
		if len(fingerprint) != sha256.Size*2 {
			return archiveCheckpoint{}, saferr.New(saferr.CategoryInternal, "archive checkpoint is invalid")
		}
		if _, err := hex.DecodeString(fingerprint); err != nil {
			return archiveCheckpoint{}, saferr.New(saferr.CategoryInternal, "archive checkpoint is invalid")
		}
	}
	return checkpoint, nil
}

func (r *Reconciler) setCheckpoint(ctx context.Context, checkpoint archiveCheckpoint) error {
	value, err := json.Marshal(checkpoint)
	if err != nil {
		return reconcileError("cannot encode archive checkpoint", err)
	}
	if _, err := r.db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at)
		VALUES (?, ?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value,
		updated_at = excluded.updated_at`, checkpointKey, string(value), time.Now().UTC().Format(checkpointTimestampLayout)); err != nil {
		return reconcileError("cannot save archive checkpoint", err)
	}
	return nil
}

func (r *Reconciler) clearCheckpoint(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", checkpointKey); err != nil {
		return reconcileError("cannot clear archive checkpoint", err)
	}
	return nil
}

func fingerprintCursor(cursor string) string {
	identifier := cursor
	if cursor == "" {
		identifier = initialCursorIdentifier
	} else if parsed, err := url.Parse(cursor); err == nil && parsed.RawQuery == "" && strings.HasSuffix(parsed.Path, "/api/documents/") {
		identifier = initialCursorIdentifier
	}
	sum := sha256.Sum256([]byte(identifier))
	return hex.EncodeToString(sum[:])
}

func slicesContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func paginationError(message string) error {
	return saferr.New(saferr.CategoryPaperless, message)
}

func reconcileError(message string, cause error) error {
	var safeError *saferr.Error
	if errors.As(cause, &safeError) {
		return saferr.Wrap(safeError.Category(), message, cause)
	}
	return saferr.Wrap(saferr.CategoryInternal, message, cause)
}
