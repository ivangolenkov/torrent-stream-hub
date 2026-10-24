package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestShutdownProcessHelper(t *testing.T) {
	if os.Getenv("HUB_TEST_SHUTDOWN_CHILD") != "1" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("hub", flag.ExitOnError)
	os.Args = []string{"hub"}
	if err := run(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessShutdownWithOpenSSEAndBodyWait(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this process integration uses POSIX SIGTERM; Windows storage has a separate native suite")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestShutdownProcessHelper$")
	cmd.Env = append(os.Environ(), "HUB_TEST_SHUTDOWN_CHILD=1", "HUB_PORT="+strconv.Itoa(port), "HUB_TORRENT_PORT=0",
		"HUB_DOWNLOAD_DIR="+filepath.Join(dir, "downloads"), "HUB_DB_PATH="+filepath.Join(dir, "hub.db"), "HUB_LOG_DIR="+filepath.Join(dir, "logs"),
		"HUB_BT_DISABLE_DHT=true", "HUB_BT_DISABLE_PEX=true", "HUB_BT_DISABLE_UPNP=true", "HUB_BT_DISABLE_UTP=true", "HUB_BT_PUBLIC_IP_DISCOVERY_ENABLED=false", "HUB_BT_RETRACKERS_MODE=off", "HUB_MIN_FREE_SPACE_GB=0")
	var childOutput bytes.Buffer
	cmd.Stdout = &childOutput
	cmd.Stderr = &childOutput
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	defer func() { cancel(); <-done }()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := &http.Client{Timeout: time.Second}
	for {
		response, err := client.Get(base + "/echo")
		if err == nil {
			response.Body.Close()
			break
		}
		select {
		case err := <-done:
			t.Fatalf("child exited before listen: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	hash := "0123456789abcdef0123456789abcdef01234567"
	response, err := client.Post(base+"/api/v1/torrent/add", "application/json", strings.NewReader(`{"link":"magnet:?xt=urn:btih:`+hash+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("add: %s %s", response.Status, content)
	}
	sseCtx, stopSSE := context.WithCancel(ctx)
	defer stopSSE()
	request, _ := http.NewRequestWithContext(sseCtx, "GET", base+"/api/v1/events", nil)
	sse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer sse.Body.Close()
	if _, err := bufio.NewReader(sse.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	// Expect/continue proves the server has entered the body read before SIGTERM.
	bodyConn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer bodyConn.Close()
	bodyConn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(bodyConn, "POST /cache HTTP/1.1\r\nHost: localhost\r\nContent-Length: 20\r\nExpect: 100-continue\r\n\r\n")
	interim, err := http.ReadResponse(bufio.NewReader(bodyConn), nil)
	if err != nil || interim.StatusCode != 100 {
		t.Fatalf("body reader not admitted: %v %v", interim, err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Signal(syscall.SIGQUIT)
		<-done
		t.Fatalf("shutdown did not cancel/join active HTTP and workers:\n%s", childOutput.String())
	}
}
