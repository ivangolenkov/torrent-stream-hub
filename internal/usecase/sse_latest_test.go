package usecase

import (
	"sync"
	"testing"
)

func TestSSELatestSnapshotAndConcurrentClose(t *testing.T) {
	sw := NewSyncWorker(nil, nil)
	slow, fast := make(chan []byte, 1), make(chan []byte, 1)
	sw.AddClient(slow)
	sw.AddClient(fast)
	sw.publish([]byte("old"))
	sw.publish([]byte("latest"))
	if string(<-slow) != "latest" || string(<-fast) != "latest" {
		t.Fatal("stale backlog retained")
	}
	newClient := make(chan []byte, 1)
	sw.AddClient(newClient)
	if string(<-newClient) != "latest" {
		t.Fatal("new subscriber missing snapshot")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); sw.publish([]byte("next")); sw.RemoveClient(fast); sw.Close() }()
	}
	wg.Wait()
	sw.RemoveClient(slow)
	sw.RemoveClient(slow)
	after := make(chan []byte, 1)
	sw.AddClient(after)
	if _, ok := <-after; ok {
		t.Fatal("subscriber accepted after Close")
	}
}
