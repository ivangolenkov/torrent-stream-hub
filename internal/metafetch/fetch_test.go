package metafetch

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(c context.Context, n, h string) ([]netip.Addr, error) {
	return f(c, n, h)
}

func TestDefaultPolicyRejectsNonPublicDestinations(t *testing.T) {
	f, _ := New("")
	f.DialContext = func(context.Context, string, string) (net.Conn, error) {
		t.Error("dialed forbidden destination")
		return nil, ErrPolicy
	}
	for _, host := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.100.100.200", "0.0.0.0", "224.0.0.1", "[::1]", "[fc00::1]", "[fe80::1]", "[::ffff:127.0.0.1]"} {
		if _, _, err := f.Fetch(context.Background(), "http://"+host+"/torrent"); !errors.Is(err, ErrPolicy) {
			t.Errorf("%s: %v", host, err)
		}
	}
	for _, link := range []string{"file:///etc/passwd", "ftp://public.example/file", "http://user:secret@public.example/file"} {
		if _, _, err := f.Fetch(context.Background(), link); !errors.Is(err, ErrPolicy) {
			t.Fatal(link, err)
		}
	}
}

func TestAllowedLANMagnetRedirectAndForbiddenRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/magnet" {
			w.Header().Set("Location", "magnet:?xt=urn:btih:0000000000000000000000000000000000000001")
		} else {
			w.Header().Set("Location", "http://169.254.169.254/credentials")
		}
		w.WriteHeader(302)
	}))
	defer server.Close()
	f, _ := New("127.0.0.1/32")
	link, body, err := f.Fetch(context.Background(), server.URL+"/magnet")
	if err != nil || len(body) != 0 || !strings.HasPrefix(link, "magnet:") {
		t.Fatal(link, err)
	}
	if _, _, err := f.Fetch(context.Background(), server.URL+"/blocked"); !errors.Is(err, ErrPolicy) {
		t.Fatal("redirect bypassed policy", err)
	}
}

func TestDNSIsPinnedAcrossRedirectsAndProxyIsIgnored(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		if r.URL.Path == "/first" {
			w.Header().Set("Location", "/last")
			w.WriteHeader(302)
			return
		}
		io.WriteString(w, "metainfo")
	}))
	defer server.Close()
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	f, _ := New("")
	var lookups, dials atomic.Int32
	f.Resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		if lookups.Add(1) > 1 {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	})
	f.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "8.8.8.8:80" {
			t.Errorf("unchecked dial: %s", address)
		}
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	_, body, err := f.Fetch(context.Background(), "http://source.example/first")
	if err != nil || string(body) != "metainfo" || lookups.Load() != 1 || dials.Load() != 2 {
		t.Fatal(string(body), err, lookups.Load(), dials.Load())
	}
}

func TestChunkedLimitRedirectLimitAndCancellation(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/large":
			w.(http.Flusher).Flush()
			io.WriteString(w, "123456789")
		case "/loop":
			w.Header().Set("Location", "/loop")
			w.WriteHeader(302)
		case "/slow":
			close(entered)
			<-r.Context().Done()
		}
	}))
	defer server.Close()
	f, _ := New("127.0.0.1/32")
	f.Limit = 8
	if _, _, err := f.Fetch(context.Background(), server.URL+"/large"); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if _, _, err := f.Fetch(context.Background(), server.URL+"/loop"); err == nil {
		t.Fatal("unbounded redirects")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := f.Fetch(ctx, server.URL+"/slow"); done <- err }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("fetch ignored cancellation")
	}
}
