// Package finalize applies OCR output and dispatches metadata processing.
package finalize

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/paperless"
	"github.com/nosovk/paperless-ai-ocr/internal/queue"
	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
	"github.com/nosovk/paperless-ai-ocr/internal/worker"
)

const (
	completeTagName             = "ai-ocr-complete"
	failedTagName               = "ai-ocr-failed"
	defaultLeaseDuration        = 5 * time.Minute
	defaultRetryDelay           = time.Minute
	transitionTimeout           = 5 * time.Second
	retryDiagnostic             = "OCR finalization interrupted"
	failedDiagnostic            = "OCR processing failed"
	uncertainDispatchDiagnostic = "Paperless AI dispatch outcome is uncertain"
)

var errSourceChanged = errors.New("source changed")

// Store is the lease-fenced durable finalization contract.
type Store interface {
	RenewLeaseContext(context.Context, int64, int, string, time.Duration) error
	AcquireFinalizationContext(context.Context, int64, int, string, string, time.Duration) (queue.FinalizationState, error)
	RenewFinalizationContext(context.Context, int64, int, string, string, time.Duration) error
	FinalizationStateContext(context.Context, int64, int, string, string) (queue.FinalizationState, error)
	AdvanceFinalizationContext(context.Context, int64, int, string, string, queue.FinalizationStage, queue.FinalizationStage) error
	SetDispatchStateContext(context.Context, int64, int, string, string, queue.DispatchState, queue.DispatchState) error
	CompleteContext(context.Context, int64, int, string) error
	FailContext(context.Context, int64, int, string, queue.SafeDiagnostic) error
	ScheduleRetryContext(context.Context, int64, int, string, time.Time, queue.SafeDiagnostic) error
}

// Paperless applies document content and tag mutations.
type Paperless interface {
	GetDocument(context.Context, int) (paperless.Document, error)
	UpdateContent(context.Context, int, string) error
	EnsureTag(context.Context, string) (paperless.Tag, error)
	UpdateTags(context.Context, int, []int, []int, []int) error
}

// Dispatcher invokes downstream Paperless AI metadata processing.
type Dispatcher interface {
	Dispatch(context.Context, int) error
}

// Options injects finalization dependencies and timing.
type Options struct {
	Store         Store
	Paperless     Paperless
	Dispatcher    Dispatcher
	LeaseDuration time.Duration
	RetryDelay    time.Duration
	Now           func() time.Time
	Token         func() (string, error)
}

// Finalizer applies exactly one already claimed result at a time.
type Finalizer struct {
	options Options
	active  atomic.Bool
}

type lostLeaseError struct{}

func (*lostLeaseError) Error() string { return "active job lease was lost" }

type ambiguousDispatchError struct{ cause error }

func (*ambiguousDispatchError) Error() string     { return "Paperless AI dispatch outcome is uncertain" }
func (err *ambiguousDispatchError) Unwrap() error { return err.cause }

type admissionError struct{ cause error }

func (*admissionError) Error() string     { return "finalization admission failed" }
func (err *admissionError) Unwrap() error { return err.cause }

// New validates finalizer dependencies.
func New(options Options) (*Finalizer, error) {
	if options.Store == nil || options.Paperless == nil || options.Dispatcher == nil {
		return nil, saferr.New(saferr.CategoryConfiguration, "invalid finalizer configuration")
	}
	if options.LeaseDuration == 0 {
		options.LeaseDuration = defaultLeaseDuration
	}
	if options.RetryDelay == 0 {
		options.RetryDelay = defaultRetryDelay
	}
	if options.LeaseDuration <= 0 || options.RetryDelay <= 0 {
		return nil, saferr.New(saferr.CategoryConfiguration, "invalid finalizer configuration")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Token == nil {
		options.Token = randomToken
	}
	return &Finalizer{options: options}, nil
}

// Process applies successful OCR output and completes the queue job.
func (finalizer *Finalizer) Process(ctx context.Context, job queue.Job, result worker.Result) error {
	if !finalizer.admit() {
		return saferr.New(saferr.CategoryValidation, "finalizer is already processing a job")
	}
	defer finalizer.active.Store(false)
	if err := validateSuccessInput(ctx, job, result); err != nil {
		return err
	}
	token, err := finalizer.options.Token()
	if err != nil || strings.TrimSpace(token) == "" {
		return saferr.New(saferr.CategoryInternal, "cannot create finalization admission")
	}
	processCtx, cancel := context.WithCancel(ctx)
	heartbeatDone := make(chan error, 1)
	go finalizer.heartbeat(processCtx, job, token, cancel, heartbeatDone)
	err = finalizer.process(processCtx, job, result, token)
	cancel()
	if heartbeatErr := <-heartbeatDone; heartbeatErr != nil {
		err = heartbeatErr
	}
	if err == nil {
		return nil
	}
	var lostLease *lostLeaseError
	if errors.As(err, &lostLease) {
		return saferr.New(saferr.CategoryValidation, "active job lease was lost")
	}
	if isSourceChanged(err) {
		if transitionErr := finalizer.fail(job, saferr.CategoryValidation); transitionErr != nil {
			return transitionError(transitionErr)
		}
		return err
	}
	var admission *admissionError
	if errors.As(err, &admission) {
		return publicError(admission.cause)
	}
	if isAmbiguousDispatch(err) {
		if transitionErr := finalizer.fail(job, saferr.CategoryProvider, uncertainDispatchDiagnostic); transitionErr != nil {
			return transitionError(transitionErr)
		}
		return saferr.New(saferr.CategoryProvider, uncertainDispatchDiagnostic)
	}
	if transitionErr := finalizer.retry(job); transitionErr != nil {
		return transitionError(transitionErr)
	}
	return publicError(err)
}

// FailOCR applies the terminal failure tag before failing the queue job.
func (finalizer *Finalizer) FailOCR(ctx context.Context, job queue.Job) error {
	if !finalizer.admit() {
		return saferr.New(saferr.CategoryValidation, "finalizer is already processing a job")
	}
	defer finalizer.active.Store(false)
	if err := validateJob(ctx, job); err != nil {
		return err
	}
	token, err := finalizer.options.Token()
	if err != nil || strings.TrimSpace(token) == "" {
		return saferr.New(saferr.CategoryInternal, "cannot create finalization admission")
	}
	processCtx, cancel := context.WithCancel(ctx)
	heartbeatDone := make(chan error, 1)
	go finalizer.heartbeat(processCtx, job, token, cancel, heartbeatDone)
	err = finalizer.failOCR(processCtx, job, token)
	cancel()
	if heartbeatErr := <-heartbeatDone; heartbeatErr != nil {
		err = heartbeatErr
	}
	if err == nil {
		return nil
	}
	var lostLease *lostLeaseError
	if errors.As(err, &lostLease) {
		return saferr.New(saferr.CategoryValidation, "active job lease was lost")
	}
	var admission *admissionError
	if errors.As(err, &admission) {
		return publicError(admission.cause)
	}
	if transitionErr := finalizer.retry(job); transitionErr != nil {
		return transitionError(transitionErr)
	}
	return publicError(err)
}

func (finalizer *Finalizer) admit() bool {
	return finalizer.active.CompareAndSwap(false, true)
}

func (finalizer *Finalizer) heartbeat(ctx context.Context, job queue.Job, token string, cancel context.CancelFunc, done chan<- error) {
	interval := finalizer.options.LeaseDuration / 3
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
			if err := finalizer.renew(ctx, job, token); err != nil {
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

func (finalizer *Finalizer) process(ctx context.Context, job queue.Job, result worker.Result, token string) error {
	if err := finalizer.renewLease(ctx, job); err != nil {
		return err
	}
	state, err := finalizer.options.Store.AcquireFinalizationContext(ctx, job.ID, job.Attempts, job.LeaseOwner, token, finalizer.options.LeaseDuration)
	if err != nil {
		return &admissionError{cause: err}
	}
	stage := state.Stage
	documentID, err := safeDocumentID(job.DocumentID)
	if err != nil {
		return err
	}
	if err := finalizer.renew(ctx, job, token); err != nil {
		return err
	}
	document, err := finalizer.options.Paperless.GetDocument(ctx, documentID)
	if err != nil {
		return err
	}
	if document.ID != documentID || document.Checksum != result.SourceChecksum {
		return saferr.Wrap(saferr.CategoryValidation, "source document changed before finalization", errSourceChanged)
	}

	if stage == queue.FinalizationPending {
		if document.Content != result.Content {
			if err := finalizer.renew(ctx, job, token); err != nil {
				return err
			}
			if err := finalizer.options.Paperless.UpdateContent(ctx, documentID, result.Content); err != nil {
				return err
			}
		}
		if err := finalizer.options.Store.AdvanceFinalizationContext(ctx, job.ID, job.Attempts, job.LeaseOwner, token, stage, queue.FinalizationContentUpdated); err != nil {
			return err
		}
		stage = queue.FinalizationContentUpdated
	}
	if stage == queue.FinalizationContentUpdated {
		if err := finalizer.mutateTag(ctx, job, token, documentID, completeTagName, true); err != nil {
			return err
		}
		if err := finalizer.options.Store.AdvanceFinalizationContext(ctx, job.ID, job.Attempts, job.LeaseOwner, token, stage, queue.FinalizationCompleteTagAdded); err != nil {
			return err
		}
		stage = queue.FinalizationCompleteTagAdded
	}
	if stage == queue.FinalizationCompleteTagAdded {
		if err := finalizer.mutateTag(ctx, job, token, documentID, failedTagName, false); err != nil {
			return err
		}
		if err := finalizer.options.Store.AdvanceFinalizationContext(ctx, job.ID, job.Attempts, job.LeaseOwner, token, stage, queue.FinalizationFailedTagRemoved); err != nil {
			return err
		}
		stage = queue.FinalizationFailedTagRemoved
	}
	if stage == queue.FinalizationFailedTagRemoved {
		if state.Dispatch == queue.DispatchReserved {
			return &ambiguousDispatchError{}
		}
		if state.Dispatch == queue.DispatchNone {
			if err := finalizer.options.Store.SetDispatchStateContext(ctx, job.ID, job.Attempts, job.LeaseOwner, token, queue.DispatchNone, queue.DispatchReserved); err != nil {
				return err
			}
			if err := finalizer.renew(ctx, job, token); err != nil {
				return err
			}
			if err := finalizer.options.Dispatcher.Dispatch(ctx, documentID); err != nil {
				if retrySafe(err) {
					if resetErr := finalizer.options.Store.SetDispatchStateContext(ctx, job.ID, job.Attempts, job.LeaseOwner, token, queue.DispatchReserved, queue.DispatchNone); resetErr != nil {
						return resetErr
					}
					return err
				}
				return &ambiguousDispatchError{cause: err}
			}
			if err := finalizer.options.Store.SetDispatchStateContext(ctx, job.ID, job.Attempts, job.LeaseOwner, token, queue.DispatchReserved, queue.DispatchConfirmed); err != nil {
				return err
			}
		}
		if err := finalizer.options.Store.AdvanceFinalizationContext(ctx, job.ID, job.Attempts, job.LeaseOwner, token, stage, queue.FinalizationMetadataDispatched); err != nil {
			return err
		}
		stage = queue.FinalizationMetadataDispatched
	}
	if stage != queue.FinalizationMetadataDispatched {
		return saferr.New(saferr.CategoryValidation, "invalid success finalization checkpoint")
	}
	if err := finalizer.renew(ctx, job, token); err != nil {
		return err
	}
	return finalizer.options.Store.CompleteContext(ctx, job.ID, job.Attempts, job.LeaseOwner)
}

func (finalizer *Finalizer) failOCR(ctx context.Context, job queue.Job, token string) error {
	if err := finalizer.renewLease(ctx, job); err != nil {
		return err
	}
	state, err := finalizer.options.Store.AcquireFinalizationContext(ctx, job.ID, job.Attempts, job.LeaseOwner, token, finalizer.options.LeaseDuration)
	if err != nil {
		return &admissionError{cause: err}
	}
	stage := state.Stage
	if !validFailureCategory(state.FailureCategory) || strings.TrimSpace(state.FailureMessage) == "" {
		return saferr.New(saferr.CategoryValidation, "terminal OCR failure intent is missing")
	}
	documentID, err := safeDocumentID(job.DocumentID)
	if err != nil {
		return err
	}
	if stage == queue.FinalizationPending {
		if err := finalizer.options.Store.AdvanceFinalizationContext(ctx, job.ID, job.Attempts, job.LeaseOwner, token, stage, queue.FinalizationFailurePending); err != nil {
			return err
		}
		stage = queue.FinalizationFailurePending
	}
	if stage == queue.FinalizationFailurePending {
		if err := finalizer.mutateTag(ctx, job, token, documentID, failedTagName, true); err != nil {
			return err
		}
		if err := finalizer.options.Store.AdvanceFinalizationContext(ctx, job.ID, job.Attempts, job.LeaseOwner, token, stage, queue.FinalizationFailureTagAdded); err != nil {
			return err
		}
		stage = queue.FinalizationFailureTagAdded
	}
	if stage != queue.FinalizationFailureTagAdded {
		return saferr.New(saferr.CategoryValidation, "invalid failure finalization checkpoint")
	}
	if err := finalizer.renew(ctx, job, token); err != nil {
		return err
	}
	return finalizer.options.Store.FailContext(ctx, job.ID, job.Attempts, job.LeaseOwner,
		queue.SafeDiagnostic{Category: state.FailureCategory, Message: state.FailureMessage})
}

func (finalizer *Finalizer) mutateTag(ctx context.Context, job queue.Job, token string, documentID int, name string, add bool) error {
	if err := finalizer.renew(ctx, job, token); err != nil {
		return err
	}
	tag, err := finalizer.options.Paperless.EnsureTag(ctx, name)
	if err != nil {
		return err
	}
	if err := finalizer.renew(ctx, job, token); err != nil {
		return err
	}
	document, err := finalizer.options.Paperless.GetDocument(ctx, documentID)
	if err != nil {
		return err
	}
	if document.ID != documentID {
		return saferr.New(saferr.CategoryValidation, "Paperless returned an unexpected document")
	}
	if add {
		if slices.Contains(document.Tags, tag.ID) {
			return nil
		}
		if err := finalizer.renew(ctx, job, token); err != nil {
			return err
		}
		return finalizer.options.Paperless.UpdateTags(ctx, documentID, document.Tags, []int{tag.ID}, nil)
	}
	if !slices.Contains(document.Tags, tag.ID) {
		return nil
	}
	if err := finalizer.renew(ctx, job, token); err != nil {
		return err
	}
	return finalizer.options.Paperless.UpdateTags(ctx, documentID, document.Tags, nil, []int{tag.ID})
}

func (finalizer *Finalizer) renewLease(ctx context.Context, job queue.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := finalizer.options.Store.RenewLeaseContext(ctx, job.ID, job.Attempts, job.LeaseOwner, finalizer.options.LeaseDuration); err != nil {
		return &lostLeaseError{}
	}
	return nil
}

func (finalizer *Finalizer) renew(ctx context.Context, job queue.Job, token string) error {
	if err := finalizer.renewLease(ctx, job); err != nil {
		return err
	}
	if err := finalizer.options.Store.RenewFinalizationContext(ctx, job.ID, job.Attempts, job.LeaseOwner, token, finalizer.options.LeaseDuration); err != nil {
		return &lostLeaseError{}
	}
	return nil
}

func (finalizer *Finalizer) retry(job queue.Job) error {
	ctx, cancel := context.WithTimeout(context.Background(), transitionTimeout)
	defer cancel()
	return finalizer.options.Store.ScheduleRetryContext(ctx, job.ID, job.Attempts, job.LeaseOwner,
		finalizer.options.Now().Add(finalizer.options.RetryDelay), queue.SafeDiagnostic{Category: saferr.CategoryInternal, Message: retryDiagnostic})
}

func (finalizer *Finalizer) fail(job queue.Job, category saferr.Category, message ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), transitionTimeout)
	defer cancel()
	diagnostic := failedDiagnostic
	if len(message) != 0 {
		diagnostic = message[0]
	}
	return finalizer.options.Store.FailContext(ctx, job.ID, job.Attempts, job.LeaseOwner,
		queue.SafeDiagnostic{Category: category, Message: diagnostic})
}

func validateSuccessInput(ctx context.Context, job queue.Job, result worker.Result) error {
	if err := validateJob(ctx, job); err != nil {
		return err
	}
	if result.JobID != job.ID || result.DocumentID != job.DocumentID || result.SourceChecksum != job.SourceChecksum ||
		strings.TrimSpace(result.SourceChecksum) == "" || strings.TrimSpace(result.DownloadSHA256) == "" || strings.TrimSpace(result.Content) == "" {
		return saferr.New(saferr.CategoryValidation, "OCR result does not match claimed job")
	}
	return nil
}

func validateJob(ctx context.Context, job queue.Job) error {
	if ctx == nil || job.ID <= 0 || job.DocumentID <= 0 || job.Attempts <= 0 || job.State != queue.StateProcessing || strings.TrimSpace(job.LeaseOwner) == "" {
		return saferr.New(saferr.CategoryValidation, "invalid claimed job")
	}
	return nil
}

func safeDocumentID(id int64) (int, error) {
	converted, err := strconv.Atoi(strconv.FormatInt(id, 10))
	if err != nil || int64(converted) != id || converted <= 0 {
		return 0, saferr.New(saferr.CategoryValidation, "document ID is outside supported range")
	}
	return converted, nil
}

func isSourceChanged(err error) bool {
	return errors.Is(err, errSourceChanged)
}

func validFailureCategory(category saferr.Category) bool {
	switch category {
	case saferr.CategoryConfiguration, saferr.CategoryPaperless, saferr.CategoryProvider,
		saferr.CategoryValidation, saferr.CategoryRendering, saferr.CategoryInternal:
		return true
	default:
		return false
	}
}

func publicError(err error) error {
	var safeErr *saferr.Error
	if errors.As(err, &safeErr) {
		return err
	}
	return saferr.New(saferr.CategoryInternal, "OCR finalization failed")
}

func transitionError(err error) error {
	var safeErr *saferr.Error
	if errors.As(err, &safeErr) {
		return saferr.New(safeErr.Category(), "job state transition failed")
	}
	return saferr.New(saferr.CategoryInternal, "job state transition failed")
}

func retrySafe(err error) bool {
	var classified interface{ RetrySafe() bool }
	return errors.As(err, &classified) && classified.RetrySafe()
}

func isAmbiguousDispatch(err error) bool {
	var ambiguous *ambiguousDispatchError
	return errors.As(err, &ambiguous)
}

func randomToken() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}
