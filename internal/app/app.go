// Package app coordinates the service lifecycle.
package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/aigate"
	"github.com/nosovk/paperless-ai-ocr/internal/config"
	"github.com/nosovk/paperless-ai-ocr/internal/database"
	"github.com/nosovk/paperless-ai-ocr/internal/finalize"
	"github.com/nosovk/paperless-ai-ocr/internal/observability"
	"github.com/nosovk/paperless-ai-ocr/internal/ocr"
	"github.com/nosovk/paperless-ai-ocr/internal/paperless"
	"github.com/nosovk/paperless-ai-ocr/internal/paperlessai"
	"github.com/nosovk/paperless-ai-ocr/internal/pdf"
	"github.com/nosovk/paperless-ai-ocr/internal/queue"
	"github.com/nosovk/paperless-ai-ocr/internal/reconcile"
	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
	"github.com/nosovk/paperless-ai-ocr/internal/server"
	"github.com/nosovk/paperless-ai-ocr/internal/worker"
)

const defaultShutdownTimeout = 10 * time.Second

const (
	defaultDatabasePath = "/app/data/paperless-ai-ocr.db"
	leaseDuration       = 5 * time.Minute
	idleInterval        = time.Second
)

var errBackground = saferr.New(saferr.CategoryInternal, "background operation failed")

// Runtime supplies initialized service operations to the lifecycle coordinator.
type Runtime interface {
	Recover(context.Context) (int64, error)
	Ping(context.Context) error
	Probe(context.Context) error
	Initialize(context.Context) error
	Reconcile(context.Context) (reconcile.Report, error)
	Claim(context.Context) (queue.Job, bool, error)
	Process(context.Context, queue.Job) (worker.Result, error)
	Terminal(context.Context, queue.Job, error) bool
	FinalizeSuccess(context.Context, queue.Job, worker.Result) error
	FinalizeFailure(context.Context, queue.Job) error
	Release(context.Context, queue.Job) error
	Active(context.Context, queue.Job) (bool, error)
	QueueDepth(context.Context) (map[queue.State]int64, error)
	Cancel()
	Close() error
}

// Options injects lifecycle dependencies and deterministic timing.
type Options struct {
	Runtime         Runtime
	Readiness       *server.Readiness
	Metrics         *observability.Metrics
	Listener        net.Listener
	HTTPServer      *http.Server
	Handler         http.Handler
	PollInterval    time.Duration
	IdleInterval    time.Duration
	ShutdownTimeout time.Duration
}

// Service contains the concrete production runtime and HTTP handler.
type Service struct {
	Runtime Runtime
	Handler http.Handler
}

type serviceRuntime struct {
	db         *sql.DB
	queue      *queue.Queue
	paperless  *paperless.Client
	ai         *aigate.Client
	dispatcher *paperlessai.Client
	reconciler *reconcile.Reconciler
	config     config.Config
	owner      string
	capability aigate.Capability
	worker     *worker.Worker
	finalizer  *finalize.Finalizer
	cancel     context.CancelCauseFunc
}

// NewService opens durable state and constructs concrete runtime dependencies.
func NewService(cfg config.Config, readiness *server.Readiness, metrics *observability.Metrics) (*Service, error) {
	if readiness == nil || metrics == nil {
		return nil, saferr.New(saferr.CategoryConfiguration, "invalid application configuration")
	}
	db, err := database.Open(filepath.Clean(defaultDatabasePath))
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = db.Close() }
	q := queue.New(db)
	paperlessClient, err := paperless.New(cfg.PaperlessURL, cfg.PaperlessAPIToken, paperless.Options{})
	if err != nil {
		cleanup()
		return nil, err
	}
	aiClient, err := aigate.New(cfg.AIBaseURL, cfg.AIAPIKey, cfg.AIModel, aigate.ClientOptions{RequestTimeout: cfg.ModelTimeout})
	if err != nil {
		cleanup()
		return nil, err
	}
	dispatcher, err := paperlessai.New(cfg.PaperlessAIWebhookURL, cfg.PaperlessURL, cfg.PaperlessAIWebhookKey, paperlessai.Options{})
	if err != nil {
		cleanup()
		return nil, err
	}
	reconciler, err := reconcile.New(db, paperlessClient, q, reconcile.Options{Model: cfg.AIModel, PromptVersion: ocr.Version})
	if err != nil {
		cleanup()
		return nil, err
	}
	owner, err := randomOwner()
	if err != nil {
		cleanup()
		return nil, err
	}
	_, cancel := context.WithCancelCause(context.Background())
	runtime := &serviceRuntime{db: db, queue: q, paperless: paperlessClient, ai: aiClient,
		dispatcher: dispatcher, reconciler: reconciler, config: cfg, owner: owner, cancel: cancel}
	webhook, err := server.New(cfg.WebhookToken, q)
	if err != nil {
		cleanup()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/health", server.NewHealthHandler(readiness))
	mux.Handle("/ready", server.NewHealthHandler(readiness))
	mux.Handle("/metrics", metrics)
	mux.Handle("/", webhook)
	return &Service{Runtime: runtime, Handler: mux}, nil
}

func (runtime *serviceRuntime) Recover(context.Context) (int64, error) {
	return runtime.queue.RecoverExpiredLeases()
}

func (runtime *serviceRuntime) Ping(ctx context.Context) error { return runtime.paperless.Ping(ctx) }

func (runtime *serviceRuntime) Probe(ctx context.Context) error {
	capability, err := runtime.ai.Probe(ctx)
	if err == nil {
		runtime.capability = capability
	}
	return err
}

func (runtime *serviceRuntime) Initialize(context.Context) error {
	inspector, err := pdf.NewInspector(pdf.InspectOptions{})
	if err != nil {
		return err
	}
	renderer, err := pdf.NewRenderer(pdf.RenderOptions{DPI: runtime.config.RenderDPI, Timeout: runtime.config.RenderTimeout})
	if err != nil {
		return err
	}
	runtime.worker, err = worker.New(worker.Options{
		Store: runtime.queue, Paperless: runtime.paperless, Capability: runtime.capability,
		Transcriber: runtime.ai, WorkspaceOptions: pdf.WorkspaceOptions{TemporaryByteBudget: runtime.config.TemporaryRenderBudget},
		Inspector: inspector, Renderer: renderer, Model: runtime.config.AIModel, BatchSize: runtime.config.BatchSize,
		RenderDPI: runtime.config.RenderDPI, ModelAttempts: runtime.config.ModelAttempts,
		LeaseDuration: leaseDuration, DocumentDeadline: runtime.config.DocumentDeadline,
	})
	if err != nil {
		return err
	}
	runtime.finalizer, err = finalize.New(finalize.Options{
		Store: runtime.queue, Paperless: runtime.paperless, Dispatcher: runtime.dispatcher, LeaseDuration: leaseDuration,
	})
	return err
}

func (runtime *serviceRuntime) Reconcile(ctx context.Context) (reconcile.Report, error) {
	return runtime.reconciler.RunOnce(ctx)
}

func (runtime *serviceRuntime) Claim(context.Context) (queue.Job, bool, error) {
	return runtime.queue.Claim(runtime.owner, leaseDuration)
}

func (runtime *serviceRuntime) Process(ctx context.Context, job queue.Job) (worker.Result, error) {
	return runtime.worker.Process(ctx, job)
}

func (runtime *serviceRuntime) Terminal(ctx context.Context, job queue.Job, _ error) bool {
	_, found, err := runtime.queue.TerminalFailureContext(ctx, job.ID, job.Attempts, job.LeaseOwner)
	return err == nil && found
}

func (runtime *serviceRuntime) FinalizeSuccess(ctx context.Context, job queue.Job, result worker.Result) error {
	return runtime.finalizer.Process(ctx, job, result)
}

func (runtime *serviceRuntime) FinalizeFailure(ctx context.Context, job queue.Job) error {
	return runtime.finalizer.FailOCR(ctx, job)
}

func (runtime *serviceRuntime) Release(ctx context.Context, job queue.Job) error {
	_, err := runtime.queue.ReleaseContext(ctx, job.ID, job.Attempts, job.LeaseOwner)
	return err
}

func (runtime *serviceRuntime) Active(ctx context.Context, job queue.Job) (bool, error) {
	return runtime.queue.ActiveContext(ctx, job.ID, job.Attempts, job.LeaseOwner)
}

func (runtime *serviceRuntime) QueueDepth(ctx context.Context) (map[queue.State]int64, error) {
	return runtime.queue.DepthContext(ctx)
}

func (runtime *serviceRuntime) Cancel() { runtime.cancel(context.Canceled) }

func (runtime *serviceRuntime) Close() error { return runtime.db.Close() }

func randomOwner() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", saferr.New(saferr.CategoryInternal, "cannot initialize worker identity")
	}
	owner := "worker-" + hex.EncodeToString(bytes[:])
	if strings.TrimSpace(owner) == "" {
		return "", saferr.New(saferr.CategoryInternal, "cannot initialize worker identity")
	}
	return owner, nil
}

// DefaultIdleInterval returns the bounded delay used when no job is available.
func DefaultIdleInterval() time.Duration { return idleInterval }

// Run starts the HTTP service, initializes dependencies, and coordinates shutdown.
func Run(parent context.Context, options Options) error {
	if parent == nil || options.Runtime == nil || options.Readiness == nil || options.Metrics == nil ||
		options.Listener == nil || options.HTTPServer == nil || options.PollInterval <= 0 || options.IdleInterval <= 0 {
		return saferr.New(saferr.CategoryConfiguration, "invalid application configuration")
	}
	shutdownTimeout := options.ShutdownTimeout
	if shutdownTimeout == 0 {
		shutdownTimeout = defaultShutdownTimeout
	}
	if shutdownTimeout < 0 {
		return saferr.New(saferr.CategoryConfiguration, "invalid application configuration")
	}

	ctx, cancel := context.WithCancelCause(context.WithoutCancel(parent))
	defer cancel(context.Canceled)
	stopParent := context.AfterFunc(parent, func() {
		options.Readiness.Set(false)
		cancel(context.Cause(parent))
	})
	defer stopParent()
	if options.Handler == nil {
		mux := http.NewServeMux()
		mux.Handle("/health", server.NewHealthHandler(options.Readiness))
		mux.Handle("/ready", server.NewHealthHandler(options.Readiness))
		mux.Handle("/metrics", options.Metrics)
		options.Handler = mux
	}
	options.HTTPServer.Handler = options.Handler
	serveDone := make(chan error, 1)
	go func() {
		err := options.HTTPServer.Serve(options.Listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveDone <- err
	}()

	var activeMu sync.Mutex
	var active queue.Job
	var loops sync.WaitGroup
	background := func(err error) {
		if err != nil && ctx.Err() == nil {
			options.Readiness.Set(false)
			cancel(errBackground)
		}
	}

	startupErr := initialize(ctx, options)
	if startupErr == nil {
		options.Readiness.Set(true)
		report, err := options.Runtime.Reconcile(ctx)
		if err != nil {
			background(err)
		} else {
			options.Metrics.RecordReconciliation(report)
		}
	}
	if startupErr == nil && ctx.Err() == nil {
		loops.Add(2)
		go func() {
			defer loops.Done()
			reconcileLoop(ctx, options, background)
		}()
		go func() {
			defer loops.Done()
			workerLoop(ctx, options, &activeMu, &active, background)
		}()
	}

	if startupErr != nil {
		cancel(saferr.New(saferr.CategoryInternal, "startup failed"))
	} else if ctx.Err() == nil {
		select {
		case err := <-serveDone:
			if err != nil {
				cancel(errBackground)
			} else {
				cancel(context.Canceled)
			}
		case <-ctx.Done():
		}
	}

	options.Readiness.Set(false)
	options.Runtime.Cancel()
	activeMu.Lock()
	claimed := active
	activeMu.Unlock()
	if claimed.ID != 0 {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		_ = options.Runtime.Release(releaseCtx, claimed)
		releaseCancel()
	}
	loops.Wait()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	shutdownErr := options.HTTPServer.Shutdown(shutdownCtx)
	shutdownCancel()
	select {
	case serveErr := <-serveDone:
		if shutdownErr == nil {
			shutdownErr = serveErr
		}
	default:
	}
	closeErr := options.Runtime.Close()
	if startupErr != nil {
		return saferr.New(saferr.CategoryInternal, "startup failed")
	}
	cause := context.Cause(ctx)
	if cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	if shutdownErr != nil || closeErr != nil {
		return saferr.New(saferr.CategoryInternal, "shutdown failed")
	}
	return nil
}

func initialize(ctx context.Context, options Options) error {
	recovered, err := options.Runtime.Recover(ctx)
	if err != nil {
		return err
	}
	options.Metrics.RecordRecoveredLeases(recovered)
	if err := options.Runtime.Ping(ctx); err != nil {
		return err
	}
	started := time.Now()
	if err := options.Runtime.Probe(ctx); err != nil {
		return err
	}
	options.Metrics.RecordProviderLatency(observability.OperationProbe, time.Since(started))
	return options.Runtime.Initialize(ctx)
}

func reconcileLoop(ctx context.Context, options Options, background func(error)) {
	ticker := time.NewTicker(options.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			report, err := options.Runtime.Reconcile(ctx)
			if err != nil {
				background(err)
				return
			}
			options.Metrics.RecordReconciliation(report)
		}
	}
}

func workerLoop(ctx context.Context, options Options, activeMu *sync.Mutex, active *queue.Job, background func(error)) {
	for ctx.Err() == nil {
		job, ok, err := options.Runtime.Claim(ctx)
		if err != nil {
			background(err)
			return
		}
		if !ok {
			if !wait(ctx, options.IdleInterval) {
				return
			}
			continue
		}
		activeMu.Lock()
		*active = job
		activeMu.Unlock()
		result, processErr := options.Runtime.Process(ctx, job)
		if processErr == nil {
			processErr = options.Runtime.FinalizeSuccess(ctx, job, result)
			if processErr == nil {
				options.Metrics.RecordJobOutcome(observability.OutcomeSuccess)
			}
		} else if options.Runtime.Terminal(ctx, job, processErr) {
			processErr = options.Runtime.FinalizeFailure(ctx, job)
			if processErr == nil {
				options.Metrics.RecordJobOutcome(observability.OutcomeFailure)
			}
		} else if !errors.Is(processErr, context.Canceled) && !errors.Is(processErr, context.DeadlineExceeded) {
			options.Metrics.RecordRetry()
		}
		activeMu.Lock()
		*active = queue.Job{}
		activeMu.Unlock()
		if processErr != nil && ctx.Err() == nil {
			stillActive, err := options.Runtime.Active(ctx, job)
			if err != nil || stillActive {
				background(processErr)
				return
			}
		}
		if depth, err := options.Runtime.QueueDepth(ctx); err == nil {
			options.Metrics.SetQueueDepth(depth)
		} else if ctx.Err() == nil {
			background(err)
			return
		}
	}
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
