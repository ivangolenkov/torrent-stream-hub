package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

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
	cfg := config.Load()
	if err := cfg.ValidateTemporaryTorrents(); err != nil {
		return err
	}
	if err := cfg.ValidateLogging(); err != nil {
		return err
	}
	files, err := logging.NewFiles(logging.FileOptions{Dir: cfg.LogDir, RetentionDays: cfg.LogRetentionDays})
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
	logging.Infof("logging initialized level=%s", logging.CurrentLevel())

	if err := ensureRuntimeDirs(cfg.DownloadDir, cfg.DBPath); err != nil {
		return fmt.Errorf("failed to prepare runtime directories: %w", err)
	}

	db, err := repository.NewSQLiteDB(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	defer db.Close()

	eng, err := engine.New(cfg)
	if err != nil {
		return fmt.Errorf("failed to initialize torrent engine: %w", err)
	}
	defer eng.Close()

	repo := repository.NewTorrentRepo(db)
	uc := usecase.NewTorrentUseCase(eng, repo)
	if err := uc.SetTemporaryLimit(cfg.MaxTemporaryTorrents); err != nil {
		return err
	}
	if err := uc.RestoreTorrents(); err != nil {
		logging.Warnf("failed to restore persisted torrents: %v", err)
	}
	uc.CleanupTemporary("startup")
	syncWorker := usecase.NewSyncWorker(eng, repo)
	workerCtx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	cleanupDone := make(chan struct{})
	go func() { defer close(cleanupDone); uc.RunTemporaryCleanup(workerCtx) }()
	go func() { defer close(workerDone); syncWorker.StartContext(workerCtx) }()
	defer func() { stopWorker(); <-workerDone; <-cleanupDone }()

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           deliveryhttp.NewRouterWithLogs(uc, syncWorker, files, web.DistFS()),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logging.Infof("Torrent-Stream-Hub listening on :%s", cfg.Port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stop)
	select {
	case <-stop:
	case err := <-serverErrors:
		return fmt.Errorf("server failed: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logging.Errorf("server shutdown failed: %v", err)
		_ = server.Close()
	}
	return nil
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
