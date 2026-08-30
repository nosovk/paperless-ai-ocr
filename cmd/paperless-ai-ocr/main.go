package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/app"
	"github.com/nosovk/paperless-ai-ocr/internal/buildinfo"
	"github.com/nosovk/paperless-ai-ocr/internal/config"
	"github.com/nosovk/paperless-ai-ocr/internal/observability"
	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
	"github.com/nosovk/paperless-ai-ocr/internal/securelog"
	"github.com/nosovk/paperless-ai-ocr/internal/server"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 10 * time.Second
	idleTimeout       = 60 * time.Second
	logQueueCapacity  = 256
	logCloseTimeout   = 100 * time.Millisecond
)

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--version" {
		metadata := buildinfo.Current()
		if _, err := fmt.Fprintf(stdout,
			"paperless-ai-ocr version=%s revision=%s build_time=%s\n",
			metadata.Version,
			metadata.Revision,
			metadata.BuildTime,
		); err != nil {
			_, _ = io.WriteString(stderr, "paperless-ai-ocr: output failed\n")
			return 1
		}
		return 0
	}
	logger, err := securelog.NewAsync(stderr, logQueueCapacity)
	if err != nil {
		_, _ = io.WriteString(stderr, "paperless-ai-ocr: logging initialization failed\n")
		return 1
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), logCloseTimeout)
		defer cancel()
		_ = logger.Close(ctx)
	}()
	cfg, err := config.Load()
	if err != nil {
		_ = logger.BackgroundFailure(saferr.CategoryConfiguration)
		return 1
	}
	readiness := server.NewReadiness()
	metrics := observability.NewMetrics()
	service, err := app.NewService(cfg, readiness, metrics)
	if err != nil {
		_ = logger.BackgroundFailure(saferr.CategoryInternal)
		return 1
	}
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.HTTPPort))
	if err != nil {
		_ = service.Runtime.Close()
		_ = logger.BackgroundFailure(saferr.CategoryInternal)
		return 1
	}
	rawSignalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	signalCtx, signalStopped := normalizeSignalCancellation(rawSignalCtx, stop)
	err = app.Run(signalCtx, app.Options{
		Runtime: service.Runtime, Readiness: readiness, Metrics: metrics, Listener: listener,
		HTTPServer: productionHTTPServer(service.Handler), Handler: service.Handler,
		PollInterval: cfg.PollInterval, IdleInterval: app.DefaultIdleInterval(),
		Logger: logger,
	})
	stop()
	<-signalStopped
	if err != nil {
		return 1
	}
	return 0
}

func productionHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
}

func normalizeSignalCancellation(signalCtx context.Context, stop func()) (context.Context, <-chan struct{}) {
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(signalCtx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-signalCtx.Done()
		stop()
		cancel(context.Canceled)
	}()
	return ctx, done
}
