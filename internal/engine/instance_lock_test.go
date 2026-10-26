package engine

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/payload"
)

func TestEngineInstanceLockLifetime(t *testing.T) {
	cfg := &config.Config{DownloadDir: t.TempDir(), BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true, BTDisableTCP: true, BTDisableUTP: true, BTRetrackersMode: "off"}
	first, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	if !first.StartTask(func(ctx context.Context) { close(entered); <-ctx.Done(); <-release }) {
		t.Fatal("task admission refused")
	}
	<-entered
	closed := make(chan struct{})
	go func() { first.Close(); close(closed) }()
	<-first.Context().Done()
	secondCfg := *cfg
	second, err := New(&secondCfg)
	if second != nil {
		second.Close()
	}
	if !errors.Is(err, payload.ErrInstanceLocked) {
		close(release)
		<-closed
		t.Fatal("shutdown released lock before joining tasks", err)
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	secondCfg = *cfg
	second, err = New(&secondCfg)
	if err != nil {
		t.Fatal("lock not released after storage close", err)
	}
	second.Close()
}

func TestEngineInstanceLockReleasedOnInitFailure(t *testing.T) {
	occupied, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	cfg := &config.Config{DownloadDir: t.TempDir(), TorrentPort: occupied.Addr().(*net.TCPAddr).Port, BTDisableDHT: true, BTDisablePEX: true, BTDisableUPNP: true, BTDisableUTP: true, BTDisableIPv6: true, BTRetrackersMode: "off"}
	eng, err := New(cfg)
	if eng != nil {
		eng.Close()
	}
	if err == nil {
		t.Fatal("expected client listen failure")
	}
	lock, err := payload.LockInstance(cfg.DownloadDir)
	if err != nil {
		t.Fatal("failed initialization leaked instance lock", err)
	}
	lock.Close()
}
