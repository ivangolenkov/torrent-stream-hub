package engine

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/payload"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
)

func (e *Engine) newTorrentClient() (*torrent.Client, storage.ClientImplCloser, error) {
	pieceCompletion, completionBackend, completionPersistent, completionErr := e.newPieceCompletion()
	root, err := os.OpenRoot(e.cfg.DownloadDir)
	if err != nil {
		pieceCompletion.Close()
		return nil, nil, err
	}
	var onRead func(string, int64)
	var onEvent func(string, string)
	if e.storageIOObserver != nil {
		onRead = func(path string, n int64) { e.storageIOObserver("read", path, n) }
		onEvent = func(operation, path string) { e.storageIOObserver(operation, path, 0) }
	}
	torrentStorage := storage.NewFileOpts(storage.NewFileClientOpts{
		IORead: onRead, IOEvent: onEvent,
		ClientBaseDir: e.cfg.DownloadDir, RootFS: root, VerifyOnOpen: true, PieceCompletion: pieceCompletion, IOContext: e.ctx,
		UsePartFiles: g.Some(false), ImportPartFiles: true,
		ManagedPayload:   true,
		DisablePieceSync: e.cfg.BTDisablePieceSync,
		IOGuard:          e.checkStorageIO,
		ResumeHooks:      e.resumeMutationHooks,
		TorrentPathResolver: func(base string, info *metainfo.Info, hash metainfo.Hash) (string, []string, error) {
			layout, err := e.StorageLayout(hash.HexString(), info)
			if err != nil {
				return "", nil, err
			}
			var paths []string
			for _, file := range info.UpvertedFiles() {
				path, err := layout.Relative(payload.Path(info, &file))
				if err != nil {
					return "", nil, err
				}
				paths = append(paths, path)
			}
			return filepath.Join(base, layout.Root), paths, nil
		},
	})
	e.mu.Lock()
	e.pieceCompletionBackend = completionBackend
	e.pieceCompletionPersistent = completionPersistent
	e.pieceCompletionErr = completionErr
	e.mu.Unlock()
	clientConfig := buildClientConfig(e.cfg)
	clientConfig.DefaultStorage = torrentStorage
	clientConfig.MaxActivePieceHashers = 2
	clientConfig.HashBudget = sharedVerificationBudget
	clientConfig.PieceHashersPerTorrent = 1
	clientConfig.Callbacks.StatusUpdated = append(clientConfig.Callbacks.StatusUpdated, func(event torrent.StatusUpdatedEvent) {
		owner, _ := e.statusTorrents.Load(strings.ToLower(event.InfoHash))
		if owner == nil {
			return
		}
		mt, ok := owner.(*ManagedTorrent)
		if !ok {
			return
		}
		event.Url = truncateDiagnostic(event.Url)
		if event.Error != nil {
			event.Error = fmt.Errorf("%s", sanitizeDiagnosticError(event.Error))
		}
		e.queueEvent(mt, "status", func(context.Context) {
			e.admissionMu.Lock()
			defer e.admissionMu.Unlock()
			if mt, ok := owner.(*ManagedTorrent); ok && e.currentTorrent(mt) {
				e.handleStatusEvent(event, mt)
			}
		})
	})
	clientConfig.Callbacks.PeerConnAdded = append(clientConfig.Callbacks.PeerConnAdded, func(conn *torrent.PeerConn) {
		e.logPeerConnEvent("added", conn)
	})
	clientConfig.Callbacks.PeerConnClosed = func(conn *torrent.PeerConn) {
		e.logPeerConnEvent("closed", conn)
	}
	client, err := torrent.NewClient(clientConfig)
	if err != nil {
		if closeErr := torrentStorage.Close(); closeErr != nil {
			logging.Warnf("failed to close torrent storage after client init error: %v", closeErr)
		}
		return nil, nil, err
	}
	return client, torrentStorage, nil
}

func buildClientConfig(cfg *config.Config) *torrent.ClientConfig {
	config.ApplyDefaults(cfg)
	clientConfig := torrent.NewDefaultClientConfig()
	clientConfig.Slogger = logging.SlogLogger()
	clientConfig.DataDir = cfg.DownloadDir
	clientConfig.ListenPort = cfg.TorrentPort
	clientConfig.Seed = cfg.BTSeed
	clientConfig.NoUpload = cfg.BTNoUpload
	clientConfig.NoDHT = cfg.BTDisableDHT
	clientConfig.DisablePEX = cfg.BTDisablePEX
	clientConfig.NoDefaultPortForwarding = cfg.BTDisableUPNP
	clientConfig.DisableTCP = cfg.BTDisableTCP
	clientConfig.DisableUTP = cfg.BTDisableUTP
	clientConfig.DisableIPv6 = cfg.BTDisableIPv6
	clientConfig.EstablishedConnsPerTorrent = cfg.BTEstablishedConns
	clientConfig.HalfOpenConnsPerTorrent = cfg.BTHalfOpenConns
	clientConfig.TotalHalfOpenConns = cfg.BTTotalHalfOpen
	clientConfig.TorrentPeersLowWater = cfg.BTPeersLowWater
	clientConfig.TorrentPeersHighWater = cfg.BTPeersHighWater
	clientConfig.DialRateLimiter = rate.NewLimiter(rate.Limit(cfg.BTDialRateLimit), cfg.BTDialRateLimit)
	clientConfig.UpnpID = "Torrent-Stream-Hub"

	// Use aggressive dial timeouts to quickly cycle through unresponsive/NAT-blocked peers
	// and free up half-open slots for potentially good peers.
	clientConfig.NominalDialTimeout = 5 * time.Second
	clientConfig.MinDialTimeout = 2 * time.Second
	clientConfig.HandshakesTimeout = 20 * time.Second // Give TCP peers more time to send the initial handshake

	clientConfig.PieceHashersPerTorrent = 4 // Increase hasher workers to prevent CPU bottleneck on fast connections

	applyPublicIPConfig(clientConfig, cfg)

	if strings.EqualFold(cfg.BTClientProfile, "qbittorrent") || cfg.BTClientProfile == "" {
		applyQBittorrentProfile(clientConfig)
	} else if !strings.EqualFold(cfg.BTClientProfile, "default") {
		logging.Warnf("unknown bt client profile %q, using qbittorrent", cfg.BTClientProfile)
		applyQBittorrentProfile(clientConfig)
	}

	if cfg.DownloadLimit > 0 {
		clientConfig.DownloadRateLimiter = rate.NewLimiter(rate.Limit(cfg.DownloadLimit), cfg.DownloadLimit)
	}
	if cfg.UploadLimit > 0 {
		clientConfig.UploadRateLimiter = rate.NewLimiter(rate.Limit(cfg.UploadLimit), max(cfg.UploadLimit, 64<<10))
	}

	return clientConfig
}

func applyQBittorrentProfile(clientConfig *torrent.ClientConfig) {
	const (
		userAgent = "qBittorrent/4.3.9"
		peerID    = "-qB4390-"
	)
	clientConfig.HTTPUserAgent = userAgent
	clientConfig.ExtendedHandshakeClientVersion = userAgent
	clientConfig.Bep20 = peerID
	clientConfig.PeerID = randomPeerID(peerID)
}

func applyPublicIPConfig(clientConfig *torrent.ClientConfig, cfg *config.Config) {
	cfg.BTPublicIPv4Status = config.PublicIPStatus(cfg.BTPublicIPv4, false)
	cfg.BTPublicIPv6Status = config.PublicIPStatus(cfg.BTPublicIPv6, true)
	if status := config.PublicIPStatus(cfg.BTPublicIPv4, false); status == "configured" {
		clientConfig.PublicIp4 = net.ParseIP(strings.TrimSpace(cfg.BTPublicIPv4)).To4()
	}
	if status := config.PublicIPStatus(cfg.BTPublicIPv6, true); status == "configured" {
		clientConfig.PublicIp6 = net.ParseIP(strings.TrimSpace(cfg.BTPublicIPv6)).To16()
	}
	if !cfg.BTPublicIPDiscoveryEnabled {
		return
	}
	if clientConfig.PublicIp4 == nil {
		if ip := discoverPublicIP("https://api.ipify.org", false); ip != nil {
			clientConfig.PublicIp4 = ip
			cfg.BTPublicIPv4Status = "discovered"
		} else if cfg.BTPublicIPv4Status == "disabled" {
			cfg.BTPublicIPv4Status = "failed"
		}
	}
	if clientConfig.PublicIp6 == nil && !cfg.BTDisableIPv6 {
		if ip := discoverPublicIP("https://api64.ipify.org", true); ip != nil {
			clientConfig.PublicIp6 = ip
			cfg.BTPublicIPv6Status = "discovered"
		} else if cfg.BTPublicIPv6Status == "disabled" {
			cfg.BTPublicIPv6Status = "failed"
		}
	}
}

func discoverPublicIP(endpoint string, ipv6 bool) net.IP {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(endpoint)
	if err != nil {
		logging.Debugf("public ip discovery failed endpoint=%s: %v", logging.SafeURLSummary(endpoint), err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logging.Debugf("public ip discovery failed endpoint=%s status=%d", logging.SafeURLSummary(endpoint), resp.StatusCode)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return nil
	}
	ip := net.ParseIP(strings.TrimSpace(string(body)))
	if ip == nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return nil
	}
	if ipv6 {
		if ip.To4() != nil || ip.To16() == nil {
			return nil
		}
		return ip.To16()
	}
	return ip.To4()
}

func randomPeerID(prefix string) string {
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		logging.Warnf("failed to generate random peer id, using deterministic fallback: %v", err)
		return (prefix + "00000000000000000000")[:20]
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(randomBytes)
	return (prefix + encoded)[:20]
}

func (e *Engine) handleStatusEvent(event torrent.StatusUpdatedEvent, mt *ManagedTorrent) {
	hash := strings.ToLower(event.InfoHash)
	tracker := logging.SafeURLSummary(event.Url)
	if event.Error != nil {
		logging.Warnf("torrent status event=%s hash=%s tracker=%s error=%v", event.Event, hash, tracker, event.Error)
	} else {
		logging.Debugf("torrent status event=%s hash=%s tracker=%s", event.Event, hash, tracker)
	}

	if hash == "" {
		return
	}

	mt.mu.Lock()
	defer mt.mu.Unlock()

	switch event.Event {
	case torrent.TrackerConnected, torrent.TrackerAnnounceSuccessful:
		mt.trackerStatus = string(event.Event)
		mt.trackerError = ""
	case torrent.TrackerDisconnected:
		mt.trackerStatus = string(event.Event)
	case torrent.TrackerAnnounceError:
		mt.trackerStatus = string(event.Event)
		if event.Error != nil {
			mt.trackerError = logging.SanitizeText(event.Error.Error())
		}
	}
}

func (e *Engine) logPeerConnEvent(event string, conn *torrent.PeerConn) {
	if conn == nil || conn.Torrent() == nil {
		return
	}

	t := conn.Torrent()
	logging.Debugf("peer connection %s hash=%s source=%s", event, t.InfoHash().HexString(), conn.Discovery)
}
