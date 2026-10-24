package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"torrent-stream-hub/internal/completion"
	"torrent-stream-hub/internal/config"
	deliveryhttp "torrent-stream-hub/internal/delivery/http"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/repository"
	"torrent-stream-hub/internal/usecase"
	"torrent-stream-hub/web"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, logging.SanitizeText(err.Error()))
		os.Exit(1)
	}
}

func run() (runErr error) {
	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	cfg := config.Load()
	if err := cfg.ValidateResources(); err != nil {
		return err
	}
	if err := cfg.ValidateTemporaryTorrents(); err != nil {
		return err
	}
	if err := cfg.ValidateLogging(); err != nil {
		return err
	}
	files, err := logging.NewAsyncFiles(logging.FileOptions{Dir: cfg.LogDir, RetentionDays: cfg.LogRetentionDays, MaxFileBytes: cfg.LogMaxFileBytes, MaxTotalBytes: cfg.LogMaxTotalBytes})
	if err != nil {
		return err
	}
	defer files.Close()
	restoreLogging := logging.UseFiles(files)
	defer restoreLogging()
	logging.Configure(cfg.LogLevel)
	defer func() {
		if runErr != nil {
			logging.Errorf("server stopped: %v", runErr)
		}
	}()
	for _, option := range cfg.DeprecatedOptions {
		logging.Warnf("deprecated setting %s has no effect; remove it from configuration", option)
	}
	logging.Infof("logging initialized level=%s", logging.CurrentLevel())

	if err := ensureRuntimeDirs(cfg.DownloadDir, cfg.DBPath); err != nil {
		return fmt.Errorf("failed to prepare runtime directories: %w", err)
	}

	db, err := repository.NewSQLiteDB(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	defer db.Close()

	completionCache, err := completion.New(db.DB())
	if err != nil {
		return fmt.Errorf("failed to initialize completion cache: %w", err)
	}
	defer completionCache.Close() // Also join the worker if engine construction fails.
	eng, err := engine.New(cfg, engine.WithPieceCompletion(completionCache))
	if err != nil {
		return fmt.Errorf("failed to initialize torrent engine: %w", err)
	}
	defer eng.Close()

	repo := repository.NewTorrentRepo(db)
	uc := usecase.NewTorrentUseCase(eng, repo)
	if err := uc.SetTemporaryLimit(cfg.MaxTemporaryTorrents); err != nil {
		return err
	}
	syncWorker := usecase.NewSyncWorker(uc)
	defer syncWorker.Close()
	ready := make(chan struct{})
	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           deliveryhttp.NewRouterWithReadiness(uc, syncWorker, files, ready, web.DistFS()),
		BaseContext:       func(net.Listener) context.Context { return eng.Context() },
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Bind before restoring: a busy port must fail before any restore effects,
	// and slow storage must not prevent HTTP diagnostics from becoming available.
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("server failed: %w", err)
	}
	return serve(ctx, server, listener, func(workerCtx context.Context) {
		if err := uc.RestoreTorrentsContext(workerCtx); err != nil && workerCtx.Err() == nil {
			logging.Warnf("failed to restore persisted torrents: %v", err)
		}
		if workerCtx.Err() != nil {
			return
		}
		logging.Infof("startup stage=temporary-cleanup")
		uc.CleanupTemporary("startup")
		logging.Infof("startup stage=initial-snapshot")
		syncWorker.Refresh()
		if workerCtx.Err() != nil {
			return
		}
		close(ready)
		logging.Infof("startup restore complete; torrent API ready")
		cleanupDone := make(chan struct{})
		go func() { defer close(cleanupDone); uc.RunTemporaryCleanup(workerCtx) }()
		defer func() { <-cleanupDone }()
		syncWorker.StartContext(workerCtx)
	}, func() {
		eng.BeginShutdown()
		syncWorker.Close()
	})
}

// Startup and workers have one owner. Cancel before joining them, and keep the
// engine/database alive until they retire even when startup has not completed.
func serve(ctx context.Context, server *http.Server, listener net.Listener, runWorkers func(context.Context), beginShutdown func()) error {
	defer listener.Close()
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); runWorkers(workerCtx) }()
	defer func() { <-workerDone }()
	defer stopWorker()
	serverErrors := make(chan error, 1)
	go func() {
		logging.Infof("Torrent-Stream-Hub listening on %s", listener.Addr())
		serverErrors <- server.Serve(listener)
	}()

	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = fmt.Errorf("server failed: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Close long-lived SSE subscriptions before waiting for HTTP handlers.
	stopWorker()
	beginShutdown()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logging.Errorf("server shutdown failed: %v", err)
		_ = server.Close()
	}
	return serveErr
}

func ensureRuntimeDirs(downloadDir, dbPath string) error {
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return err
	}

	dbDir := filepath.Dir(dbPath)
	if dbDir == "." || dbDir == "" {
		return nil
	}

	return os.MkdirAll(dbDir, 0o755)
}
