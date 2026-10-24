// Package metafetch fetches untrusted metainfo links with per-connection address
// policy. It deliberately does not inherit environment proxy settings.
package metafetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrPolicy = errors.New("remote metainfo destination is not allowed")
var ErrTooLarge = errors.New("metainfo exceeds size limit")

type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type rule struct {
	prefix     netip.Prefix
	host, port string
}
type Fetcher struct {
	rules       []rule
	Resolver    Resolver
	DialContext func(context.Context, string, string) (net.Conn, error)
	Limit       int64
	Timeout     time.Duration
	Redirects   int
}

func New(allowlist string) (*Fetcher, error) {
	f := &Fetcher{Resolver: net.DefaultResolver, DialContext: (&net.Dialer{Timeout: 15 * time.Second}).DialContext, Limit: 32 << 20, Timeout: 60 * time.Second, Redirects: 5}
	for _, entry := range strings.Split(allowlist, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			f.rules = append(f.rules, rule{prefix: prefix.Masked()})
			continue
		}
		host, port, err := net.SplitHostPort(entry)
		if err != nil || host == "" || !validPort(port) || strings.ContainsAny(host, "/@?#%*") {
			return nil, fmt.Errorf("invalid metainfo allowlist entry (use host:port or CIDR)")
		}
		f.rules = append(f.rules, rule{host: strings.ToLower(strings.TrimSuffix(host, ".")), port: port})
	}
	return f, nil
}

func validPort(port string) bool {
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535
}
func (f *Fetcher) allowed(host, port string, ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	for _, r := range f.rules {
		if r.prefix.IsValid() && r.prefix.Contains(ip) {
			return true
		}
		if r.host == host && r.port == port {
			return true
		}
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	// Shared address space and benchmarking networks are not public destinations.
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15"} {
		if netip.MustParsePrefix(cidr).Contains(ip) {
			return false
		}
	}
	return true
}

func (f *Fetcher) Fetch(ctx context.Context, link string) (string, []byte, error) {
	if f.Limit <= 0 || f.Timeout <= 0 || f.Redirects < 0 {
		return "", nil, ErrPolicy
	}
	ctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()
	// Pin each hostname for this operation. Redirects/retries cannot trigger a
	// second unchecked DNS lookup, and the dialer always receives a literal IP.
	pinned := make(map[string][]netip.Addr)
	var pinnedMu sync.Mutex
	resolve := func(host, port string) ([]netip.Addr, error) {
		pinnedMu.Lock()
		defer pinnedMu.Unlock()
		for _, r := range host {
			if r > 127 {
				return nil, ErrPolicy
			}
		}
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		key := net.JoinHostPort(host, port)
		if ips, ok := pinned[key]; ok {
			return ips, nil
		}
		var ips []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			ips = []netip.Addr{ip}
		} else {
			var err error
			ips, err = f.Resolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, fmt.Errorf("metainfo DNS lookup failed")
			}
		}
		if len(ips) == 0 || len(ips) > 16 {
			return nil, ErrPolicy
		}
		for _, ip := range ips {
			if !f.allowed(host, port, ip) {
				return nil, ErrPolicy
			}
		}
		pinned[key] = ips
		return ips, nil
	}
	transport := &http.Transport{Proxy: nil, ResponseHeaderTimeout: 15 * time.Second, MaxResponseHeaderBytes: 64 << 10, DisableCompression: true,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, ErrPolicy
			}
			ips, err := resolve(host, port)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				var conn net.Conn
				conn, err = f.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
				if err == nil {
					return conn, nil
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
			}
			return nil, err
		}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	current, err := url.Parse(link)
	if err != nil {
		return "", nil, ErrPolicy
	}
	for redirects := 0; ; redirects++ {
		if redirects > f.Redirects {
			return "", nil, fmt.Errorf("too many metainfo redirects")
		}
		if current.Scheme == "magnet" && redirects > 0 {
			return current.String(), nil, nil
		}
		if (current.Scheme != "http" && current.Scheme != "https") || current.User != nil || current.Hostname() == "" {
			return "", nil, ErrPolicy
		}
		port := current.Port()
		if port == "" {
			if current.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		if !validPort(port) {
			return "", nil, ErrPolicy
		}
		if _, err := resolve(current.Hostname(), port); err != nil {
			return "", nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return "", nil, ErrPolicy
		}
		req.Header.Set("User-Agent", "Torrent-Stream-Hub")
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", nil, ctx.Err()
			}
			if errors.Is(err, ErrPolicy) {
				return "", nil, ErrPolicy
			}
			return "", nil, fmt.Errorf("remote metainfo request failed")
		}
		if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
			next, err := resp.Location()
			resp.Body.Close()
			if err != nil {
				return "", nil, fmt.Errorf("invalid metainfo redirect")
			}
			current = next
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", nil, fmt.Errorf("remote metainfo returned HTTP %d", resp.StatusCode)
		}
		if resp.ContentLength > f.Limit {
			resp.Body.Close()
			return "", nil, ErrTooLarge
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, f.Limit+1))
		resp.Body.Close()
		if err != nil {
			if ctx.Err() != nil {
				return "", nil, ctx.Err()
			}
			return "", nil, fmt.Errorf("remote metainfo body failed")
		}
		if int64(len(body)) > f.Limit {
			return "", nil, ErrTooLarge
		}
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) < 1<<20 && bytes.HasPrefix(bytes.ToLower(trimmed[:min(len(trimmed), 7)]), []byte("magnet:")) {
			return string(trimmed), nil, nil
		}
		return "", body, nil
	}
}
