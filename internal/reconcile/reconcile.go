// Package reconcile discovers durable work missed by webhook delivery.
package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"math"
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
	checkpointTimestampLayout = "2006-01-02T15:04:05.000000000Z07:00"
)

// Options configures one bounded reconciliation pass.
type Options struct {
	Model                  string
	PromptVersion          string
	MaxCandidatesPerPass   int
	MaxArchivePagesPerPass int
	Yield                  func(context.Context) error
}

// Report contains non-sensitive reconciliation progress counts.
type Report struct {
	CandidatesResolved int
	DocumentsSeen      int
	JobsCreated        int
	PagesProcessed     int
	ScanComplete       bool
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
	yield                  func(context.Context) error
}

// New constructs a bounded reconciler.
func New(db *sql.DB, client *paperless.Client, q *queue.Queue, options Options) (*Reconciler, error) {
	if db == nil || client == nil || q == nil || strings.TrimSpace(options.Model) == "" || strings.TrimSpace(options.PromptVersion) == "" || options.MaxCandidatesPerPass < 0 || options.MaxArchivePagesPerPass < 0 {
		return nil, saferr.New(saferr.CategoryValidation, "invalid reconciler configuration")
	}
	if options.MaxCandidatesPerPass == 0 {
		options.MaxCandidatesPerPass = defaultMaxCandidates
	}
	if options.MaxArchivePagesPerPass == 0 {
		options.MaxArchivePagesPerPass = defaultMaxArchivePages
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
			return report, reconcileError("cannot resolve candidate", err)
		}
		if strings.TrimSpace(document.Checksum) == "" {
			return report, saferr.New(saferr.CategoryPaperless, "candidate document checksum is blank")
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

	cursor, err := r.checkpoint(ctx)
	if err != nil {
		return report, err
	}
	for pageNumber := range r.maxArchivePagesPerPass {
		page, err := r.paperless.ListDocumentsPage(ctx, cursor)
		if err != nil {
			return report, reconcileError("cannot list archive page", err)
		}
		for _, document := range page.Documents {
			report.DocumentsSeen++
			if strings.TrimSpace(document.Checksum) == "" {
				return report, saferr.New(saferr.CategoryPaperless, "archive document checksum is blank")
			}
			_, created, err := r.queue.Enqueue(queue.EnqueueInput{
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
		if err := r.setCheckpoint(ctx, page.Next); err != nil {
			return report, err
		}
		report.PagesProcessed++
		cursor = page.Next
		if cursor == "" {
			report.ScanComplete = true
			return report, nil
		}
		if pageNumber+1 < r.maxArchivePagesPerPass {
			if err := r.yield(ctx); err != nil {
				return report, reconcileError("archive page yield failed", err)
			}
		}
	}
	return report, nil
}

func (r *Reconciler) checkpoint(ctx context.Context) (string, error) {
	var cursor string
	err := r.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", checkpointKey).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", reconcileError("cannot load archive checkpoint", err)
	}
	return cursor, nil
}

func (r *Reconciler) setCheckpoint(ctx context.Context, cursor string) error {
	if cursor == "" {
		if _, err := r.db.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", checkpointKey); err != nil {
			return reconcileError("cannot clear archive checkpoint", err)
		}
		return nil
	}
	if _, err := r.db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at)
		VALUES (?, ?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value,
		updated_at = excluded.updated_at`, checkpointKey, cursor, time.Now().UTC().Format(checkpointTimestampLayout)); err != nil {
		return reconcileError("cannot save archive checkpoint", err)
	}
	return nil
}

func reconcileError(message string, cause error) error {
	return saferr.Wrap(saferr.CategoryInternal, message, cause)
}
