// Package worker processes one claimed document into durable OCR checkpoints.
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/aigate"
	"github.com/nosovk/paperless-ai-ocr/internal/ocr"
	"github.com/nosovk/paperless-ai-ocr/internal/paperless"
	"github.com/nosovk/paperless-ai-ocr/internal/pdf"
	"github.com/nosovk/paperless-ai-ocr/internal/queue"
	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

const (
	sourceName              = "source.pdf"
	renderFormat            = "png"
	defaultModelAttempts    = 3
	maximumModelAttempts    = 10
	defaultLeaseDuration    = 5 * time.Minute
	defaultRetryDelay       = time.Minute
	defaultDocumentDeadline = 6 * time.Hour
	defaultBackoff          = time.Second
	maximumBackoff          = 30 * time.Second
	transitionTimeout       = 5 * time.Second
	maxDocumentPages        = 10_000
	failedDiagnostic        = "OCR processing failed"
	retryDiagnostic         = "OCR processing interrupted"
)

// Store is the parent-lease-fenced durable checkpoint contract.
type Store interface {
	RenewLeaseContext(context.Context, int64, int, string, time.Duration) error
	EnsureBatchesContext(context.Context, int64, int, string, []queue.BatchRange, int, string) ([]queue.Batch, error)
	ListBatchesContext(context.Context, int64, int, string) ([]queue.Batch, error)
	CheckpointBatchContext(context.Context, int64, int, string, queue.BatchRange, int, string, string) error
	FailContext(context.Context, int64, int, string, queue.SafeDiagnostic) error
	ScheduleRetryContext(context.Context, int64, int, string, time.Time, queue.SafeDiagnostic) error
}

// Paperless provides source metadata and streaming download only.
type Paperless interface {
	GetDocument(context.Context, int) (paperless.Document, error)
	DownloadOriginal(context.Context, int, io.Writer) error
}

// Inspector reads PDF page metadata.
type Inspector interface {
	Inspect(context.Context, *pdf.Workspace, string) (pdf.Info, error)
}

// Renderer exposes rendered files only during its callback.
type Renderer interface {
	Render(context.Context, *pdf.Workspace, string, int, int, func([]pdf.Page) error) error
}

// Options injects deterministic worker dependencies and bounds.
type Options struct {
	Store            Store
	Paperless        Paperless
	Capability       aigate.Capability
	Transcriber      aigate.Transcriber
	WorkspaceOptions pdf.WorkspaceOptions
	WorkspaceFactory func(context.Context, int64, pdf.WorkspaceOptions) (*pdf.Workspace, error)
	Inspector        Inspector
	Renderer         Renderer
	Model            string
	BatchSize        int
	RenderDPI        int
	ModelAttempts    int
	LeaseDuration    time.Duration
	RetryDelay       time.Duration
	DocumentDeadline time.Duration
	Now              func() time.Time
	Sleep            func(context.Context, time.Duration) error
	Jitter           func(time.Duration) time.Duration
	Retry            func(error) (aigate.RetryClass, time.Duration, bool)
	Unsupported      func(error) bool
}

// Worker processes exactly one already claimed job.
type Worker struct {
	options Options
	active  atomic.Bool
}

// Result is validated content awaiting Task 14 finalization.
type Result struct {
	JobID          int64
	DocumentID     int64
	SourceChecksum string
	DownloadSHA256 string
	Content        string
}

type lostLeaseError struct{}

func (*lostLeaseError) Error() string { return "active job lease was lost" }

// New validates deterministic worker dependencies.
func New(options Options) (*Worker, error) {
	if options.Store == nil || options.Paperless == nil || options.Transcriber == nil || options.Inspector == nil || options.Renderer == nil ||
		strings.TrimSpace(options.Model) == "" || options.BatchSize < 1 || options.BatchSize > 5 || options.RenderDPI <= 0 ||
		options.Capability != aigate.DirectPDF && options.Capability != aigate.PageImages {
		return nil, saferr.New(saferr.CategoryConfiguration, "invalid worker configuration")
	}
	if options.ModelAttempts == 0 {
		options.ModelAttempts = defaultModelAttempts
	}
	if options.ModelAttempts < 0 || options.ModelAttempts > maximumModelAttempts {
		return nil, saferr.New(saferr.CategoryConfiguration, "invalid worker configuration")
	}
	if options.LeaseDuration == 0 {
		options.LeaseDuration = defaultLeaseDuration
	}
	if options.RetryDelay == 0 {
		options.RetryDelay = defaultRetryDelay
	}
	if options.DocumentDeadline == 0 {
		options.DocumentDeadline = defaultDocumentDeadline
	}
	if options.LeaseDuration <= 0 || options.RetryDelay <= 0 || options.DocumentDeadline <= 0 || options.WorkspaceOptions.TemporaryByteBudget <= 0 {
		return nil, saferr.New(saferr.CategoryConfiguration, "invalid worker configuration")
	}
	if options.WorkspaceFactory == nil {
		options.WorkspaceFactory = pdf.NewWorkspace
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Sleep == nil {
		options.Sleep = sleepContext
	}
	if options.Jitter == nil {
		options.Jitter = func(time.Duration) time.Duration { return 0 }
	}
	if options.Retry == nil {
		options.Retry = aigate.Retry
	}
	if options.Unsupported == nil {
		options.Unsupported = aigate.UnsupportedAttachment
	}
	return &Worker{options: options}, nil
}

// Process builds durable validated checkpoints and returns finalization input.
// On success the parent job remains processing with an active lease.
func (worker *Worker) Process(ctx context.Context, job queue.Job) (Result, error) {
	if ctx == nil {
		return Result{}, saferr.New(saferr.CategoryValidation, "invalid claimed job")
	}
	if !worker.active.CompareAndSwap(false, true) {
		return Result{}, saferr.New(saferr.CategoryValidation, "worker is already processing a job")
	}
	defer worker.active.Store(false)
	processCtx, cancel := context.WithTimeout(ctx, worker.options.DocumentDeadline)
	heartbeatDone := make(chan error, 1)
	go worker.heartbeat(processCtx, job, cancel, heartbeatDone)
	result, err := worker.process(processCtx, job)
	cancel()
	heartbeatErr := <-heartbeatDone
	if heartbeatErr != nil {
		err = heartbeatErr
	}
	if err == nil {
		return result, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if transitionErr := worker.scheduleRetry(job); transitionErr != nil {
			return Result{}, transitionError(transitionErr)
		}
		return Result{}, err
	}
	var lostLease *lostLeaseError
	if errors.As(err, &lostLease) {
		return Result{}, saferr.New(saferr.CategoryValidation, "active job lease was lost")
	}
	safeErr := publicError(err)
	if transitionErr := worker.fail(job, category(safeErr)); transitionErr != nil {
		return Result{}, transitionError(transitionErr)
	}
	return Result{}, safeErr
}

func (worker *Worker) heartbeat(ctx context.Context, job queue.Job, cancel context.CancelFunc, done chan<- error) {
	interval := worker.options.LeaseDuration / 3
	if interval <= 0 {
		interval = time.Nanosecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- nil
			return
		case <-ticker.C:
			if err := worker.options.Store.RenewLeaseContext(ctx, job.ID, job.Attempts, job.LeaseOwner, worker.options.LeaseDuration); err != nil {
				if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
					done <- nil
					return
				}
				cancel()
				done <- &lostLeaseError{}
				return
			}
		}
	}
}

func (worker *Worker) process(ctx context.Context, job queue.Job) (_ Result, err error) {
	if ctx == nil || job.ID <= 0 || job.DocumentID <= 0 || job.Attempts <= 0 || job.State != queue.StateProcessing || strings.TrimSpace(job.LeaseOwner) == "" {
		return Result{}, saferr.New(saferr.CategoryValidation, "invalid claimed job")
	}
	if job.Model != worker.options.Model || job.PromptVersion != ocr.Version {
		return Result{}, saferr.New(saferr.CategoryValidation, "job processing contract does not match worker")
	}
	documentID, err := safeDocumentID(job.DocumentID)
	if err != nil {
		return Result{}, err
	}
	if err := worker.renew(ctx, job); err != nil {
		return Result{}, err
	}
	document, err := worker.options.Paperless.GetDocument(ctx, documentID)
	if err != nil {
		return Result{}, err
	}
	if err := worker.renew(ctx, job); err != nil {
		return Result{}, err
	}
	if document.ID != documentID || document.Checksum != job.SourceChecksum {
		return Result{}, saferr.New(saferr.CategoryValidation, "source document changed before OCR")
	}

	workspace, err := worker.options.WorkspaceFactory(ctx, job.ID, worker.options.WorkspaceOptions)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		if closeErr := workspace.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	file, err := workspace.Create(ctx, sourceName)
	if err != nil {
		return Result{}, err
	}
	path := file.Name()
	hash := sha256.New()
	downloadErr := worker.options.Paperless.DownloadOriginal(ctx, documentID, io.MultiWriter(file, hash))
	if downloadErr == nil {
		downloadErr = file.Sync()
	}
	if downloadErr != nil {
		_ = file.Abort()
		return Result{}, downloadErr
	}
	if err := file.Close(); err != nil {
		return Result{}, err
	}
	if err := worker.renew(ctx, job); err != nil {
		return Result{}, err
	}
	info, err := worker.options.Inspector.Inspect(ctx, workspace, sourceName)
	if err != nil {
		return Result{}, err
	}
	if err := worker.renew(ctx, job); err != nil {
		return Result{}, err
	}

	draft := ocr.BoundDraft(document.Content)
	capability := worker.options.Capability
	var pdfBytes []byte
	if capability == aigate.DirectPDF && info.Pages <= 5 {
		if pdfBytes, err = readDirectPDF(path); err != nil {
			return Result{}, err
		}
		if !aigate.DirectPDFEligible(info.Pages, pdfBytes) {
			capability = aigate.PageImages
			pdfBytes = nil
		}
	} else if capability == aigate.DirectPDF {
		capability = aigate.PageImages
	}
	ranges, err := planRanges(info.Pages, worker.options.BatchSize, capability)
	if err != nil {
		return Result{}, err
	}
	checkpoints, err := worker.options.Store.EnsureBatchesContext(ctx, job.ID, job.Attempts, job.LeaseOwner, ranges, worker.options.RenderDPI, renderFormat)
	if err != nil {
		return Result{}, err
	}

	validated := make([]ocr.Batch, len(checkpoints))
	for index, checkpoint := range checkpoints {
		if checkpoint.State == queue.StateCompleted {
			batch, _, validateErr := ocr.ValidateCanonical([]byte(checkpoint.ResultText), checkpoint.FirstPage, checkpoint.LastPage)
			if validateErr != nil {
				return Result{}, validateErr
			}
			validated[index] = batch
			continue
		}
		if err := worker.renew(ctx, job); err != nil {
			return Result{}, err
		}
		pageRange := ranges[index]
		var batch ocr.Batch
		if capability == aigate.DirectPDF {
			batch, err = worker.transcribeAndCheckpoint(ctx, job, pageRange, draft, pdfBytes, nil)
			if err != nil && worker.options.Unsupported(err) {
				capability = aigate.PageImages
				pdfBytes = nil
				batch, err = worker.renderTranscribeAndCheckpoint(ctx, job, workspace, pageRange, draft)
			}
		} else {
			batch, err = worker.renderTranscribeAndCheckpoint(ctx, job, workspace, pageRange, draft)
		}
		if err != nil {
			return Result{}, err
		}
		validated[index] = batch
	}
	content, err := ocr.Join(validated)
	if err != nil {
		return Result{}, err
	}
	if err := worker.renew(ctx, job); err != nil {
		return Result{}, err
	}
	return Result{JobID: job.ID, DocumentID: job.DocumentID, SourceChecksum: job.SourceChecksum,
		DownloadSHA256: hex.EncodeToString(hash.Sum(nil)), Content: content}, nil
}

func (worker *Worker) renderTranscribeAndCheckpoint(ctx context.Context, job queue.Job, workspace *pdf.Workspace, pageRange queue.BatchRange, draft string) (ocr.Batch, error) {
	var batch ocr.Batch
	err := worker.options.Renderer.Render(ctx, workspace, sourceName, pageRange.FirstPage, pageRange.LastPage, func(pages []pdf.Page) error {
		if err := worker.renew(ctx, job); err != nil {
			return err
		}
		images := make([][]byte, len(pages))
		for index, page := range pages {
			if page.Number != pageRange.FirstPage+index || page.Size <= 0 || page.Size > 8<<20 {
				return saferr.New(saferr.CategoryRendering, "rendered page output exceeded limits")
			}
			data, err := os.ReadFile(page.Path)
			if err != nil || int64(len(data)) != page.Size || !aigate.AttachmentEligible(data) {
				return saferr.New(saferr.CategoryRendering, "rendered page output could not be read safely")
			}
			images[index] = data
		}
		var err error
		batch, err = worker.transcribeAndCheckpoint(ctx, job, pageRange, draft, nil, images)
		return err
	})
	return batch, err
}

func (worker *Worker) transcribeAndCheckpoint(ctx context.Context, job queue.Job, pageRange queue.BatchRange, draft string, pdfBytes []byte, images [][]byte) (ocr.Batch, error) {
	capability := aigate.PageImages
	if len(pdfBytes) != 0 {
		capability = aigate.DirectPDF
	}
	input := aigate.Transcription{Capability: capability, FirstPage: pageRange.FirstPage, LastPage: pageRange.LastPage, OCRDraft: draft, PDF: pdfBytes, Images: images}
	var raw json.RawMessage
	var err error
	for attempt := 1; attempt <= worker.options.ModelAttempts; attempt++ {
		if err = worker.renew(ctx, job); err != nil {
			return ocr.Batch{}, err
		}
		raw, err = worker.options.Transcriber.Transcribe(ctx, input)
		if err == nil {
			break
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ocr.Batch{}, ctxErr
		}
		if worker.options.Unsupported(err) {
			return ocr.Batch{}, err
		}
		var retryAfter time.Duration
		var retryable bool
		if errors.Is(err, context.DeadlineExceeded) {
			retryable = true
		} else {
			_, retryAfter, retryable = worker.options.Retry(err)
		}
		if !retryable || attempt == worker.options.ModelAttempts {
			if errors.Is(err, context.DeadlineExceeded) {
				return ocr.Batch{}, saferr.New(saferr.CategoryProvider, "transcription service unavailable")
			}
			return ocr.Batch{}, err
		}
		delay := retryAfter
		if delay <= 0 {
			delay = backoffDelay(attempt, worker.options.Jitter)
		}
		if err := worker.options.Sleep(ctx, delay); err != nil {
			return ocr.Batch{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return ocr.Batch{}, err
	}
	batch, canonical, err := ocr.ValidateCanonical(raw, pageRange.FirstPage, pageRange.LastPage)
	if err != nil {
		return ocr.Batch{}, err
	}
	if err := worker.renew(ctx, job); err != nil {
		return ocr.Batch{}, err
	}
	if err := worker.options.Store.CheckpointBatchContext(ctx, job.ID, job.Attempts, job.LeaseOwner, pageRange, worker.options.RenderDPI, renderFormat, string(canonical)); err != nil {
		return ocr.Batch{}, err
	}
	return batch, nil
}

func backoffDelay(attempt int, jitter func(time.Duration) time.Duration) time.Duration {
	const saturationExponent = 5

	exponent := max(attempt-1, 0)
	if exponent >= saturationExponent {
		return maximumBackoff
	}
	base := defaultBackoff << uint(exponent)
	extra := jitter(base)
	if extra <= 0 {
		return base
	}
	if extra >= maximumBackoff-base {
		return maximumBackoff
	}
	return base + extra
}

func (worker *Worker) renew(ctx context.Context, job queue.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := worker.options.Store.RenewLeaseContext(ctx, job.ID, job.Attempts, job.LeaseOwner, worker.options.LeaseDuration); err != nil {
		return &lostLeaseError{}
	}
	return nil
}

func (worker *Worker) fail(job queue.Job, errorCategory saferr.Category) error {
	ctx, cancel := context.WithTimeout(context.Background(), transitionTimeout)
	defer cancel()
	return worker.options.Store.FailContext(ctx, job.ID, job.Attempts, job.LeaseOwner, queue.SafeDiagnostic{Category: errorCategory, Message: failedDiagnostic})
}

func (worker *Worker) scheduleRetry(job queue.Job) error {
	ctx, cancel := context.WithTimeout(context.Background(), transitionTimeout)
	defer cancel()
	return worker.options.Store.ScheduleRetryContext(ctx, job.ID, job.Attempts, job.LeaseOwner,
		worker.options.Now().Add(worker.options.RetryDelay), queue.SafeDiagnostic{Category: saferr.CategoryInternal, Message: retryDiagnostic})
}

func planRanges(pages, batchSize int, capability aigate.Capability) ([]queue.BatchRange, error) {
	if pages <= 0 || pages > maxDocumentPages {
		return nil, saferr.New(saferr.CategoryRendering, "PDF page count exceeded supported limit")
	}
	if capability == aigate.DirectPDF {
		return []queue.BatchRange{{FirstPage: 1, LastPage: pages}}, nil
	}
	ranges := make([]queue.BatchRange, 0, pages/batchSize+1)
	for firstPage := 1; firstPage <= pages; firstPage += batchSize {
		ranges = append(ranges, queue.BatchRange{FirstPage: firstPage, LastPage: min(firstPage+batchSize-1, pages)})
	}
	return ranges, nil
}

func readDirectPDF(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, saferr.New(saferr.CategoryRendering, "source PDF could not be read")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (8<<20)+1))
	if err != nil {
		return nil, saferr.New(saferr.CategoryRendering, "source PDF could not be read")
	}
	return data, nil
}

func safeDocumentID(id int64) (int, error) {
	converted, err := strconv.Atoi(strconv.FormatInt(id, 10))
	if err != nil || int64(converted) != id || converted <= 0 {
		return 0, saferr.New(saferr.CategoryValidation, "document ID is outside supported range")
	}
	return converted, nil
}

func category(err error) saferr.Category {
	var safeError *saferr.Error
	if errors.As(err, &safeError) {
		return safeError.Category()
	}
	return saferr.CategoryInternal
}

func publicError(err error) error {
	var safeError *saferr.Error
	if errors.As(err, &safeError) {
		return err
	}
	return saferr.New(saferr.CategoryInternal, "OCR processing failed")
}

func transitionError(err error) error {
	var safeError *saferr.Error
	if errors.As(err, &safeError) {
		return saferr.New(safeError.Category(), "job state transition failed")
	}
	return saferr.New(saferr.CategoryInternal, "job state transition failed")
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
