package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"torrent-stream-hub/internal/config"
	deliveryhttp "torrent-stream-hub/internal/delivery/http"
	"torrent-stream-hub/internal/engine"
	"torrent-stream-hub/internal/repository"
	"torrent-stream-hub/internal/usecase"
	"torrent-stream-hub/web"
)

func main() {
	cfg := config.Load()

	if err := ensureRuntimeDirs(cfg.DownloadDir, cfg.DBPath); err != nil {
		log.Fatalf("failed to prepare runtime directories: %v", err)
	}

	db, err := repository.NewSQLiteDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("failed to initialize database: %v", err)
	}
	defer db.Close()

	eng, err := engine.New(cfg)
	if err != nil {
		log.Fatalf("failed to initialize torrent engine: %v", err)
	}
	defer eng.Close()

	repo := repository.NewTorrentRepo(db)
	uc := usecase.NewTorrentUseCase(eng, repo)
	syncWorker := usecase.NewSyncWorker(eng, repo)
	go syncWorker.Start()

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           deliveryhttp.NewRouter(uc, syncWorker, web.DistFS()),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("Torrent-Stream-Hub listening on :%s", cfg.Port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server failed: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("server shutdown failed: %v", err)
	}
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
