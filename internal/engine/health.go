package engine

import (
	"path/filepath"
	"strings"
	"time"

	"torrent-stream-hub/internal/completion"
	"torrent-stream-hub/internal/config"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
)

func (e *Engine) BTHealth() *models.BTHealth {
	e.mu.RLock()
	mts := make([]*ManagedTorrent, 0, len(e.managedTorrents))
	for _, mt := range e.managedTorrents {
		mts = append(mts, mt)
	}

	health := &models.BTHealth{
		SeedEnabled:                e.cfg.BTSeed,
		UploadEnabled:              !e.cfg.BTNoUpload,
		DHTEnabled:                 !e.cfg.BTDisableDHT,
		PEXEnabled:                 !e.cfg.BTDisablePEX,
		UPNPEnabled:                !e.cfg.BTDisableUPNP,
		TCPEnabled:                 !e.cfg.BTDisableTCP,
		UTPEnabled:                 !e.cfg.BTDisableUTP,
		IPv6Enabled:                !e.cfg.BTDisableIPv6,
		ListenPort:                 e.cfg.TorrentPort,
		ClientProfile:              normalizedBTClientProfile(e.cfg.BTClientProfile),
		DownloadProfile:            e.cfg.BTDownloadProfile,
		EstablishedConnsPerTorrent: e.cfg.BTEstablishedConns,
		HalfOpenConnsPerTorrent:    e.cfg.BTHalfOpenConns,
		TotalHalfOpenConns:         e.cfg.BTTotalHalfOpen,
		PeersLowWater:              e.cfg.BTPeersLowWater,
		PeersHighWater:             e.cfg.BTPeersHighWater,
		DialRateLimit:              e.cfg.BTDialRateLimit,
		PublicIPDiscoveryEnabled:   e.cfg.BTPublicIPDiscoveryEnabled,
		PublicIPv4Status:           firstNonEmptyString(e.cfg.BTPublicIPv4Status, config.PublicIPStatus(e.cfg.BTPublicIPv4, false)),
		PublicIPv6Status:           firstNonEmptyString(e.cfg.BTPublicIPv6Status, config.PublicIPStatus(e.cfg.BTPublicIPv6, true)),
		RetrackersMode:             normalizedRetrackersMode(e.cfg.BTRetrackersMode),
		DownloadLimit:              e.cfg.DownloadLimit,
		UploadLimit:                e.cfg.UploadLimit,
		SwarmWatchdogEnabled:       e.cfg.BTSwarmWatchdogEnabled,
		SwarmCheckIntervalSec:      e.cfg.BTSwarmCheckIntervalSec,
		SwarmRefreshCooldownSec:    e.cfg.BTSwarmRefreshCooldownSec,
		InvalidMetainfoCount:       e.invalidMetainfoCount,
		PieceCompletionBackend:     e.pieceCompletionBackend,
		PieceCompletionPersistent:  e.pieceCompletionPersistent,
		PieceCompletionError:       e.pieceCompletionErr,
		PeerDropRatio:              e.cfg.BTSwarmPeerDropRatio,
		SeedDropRatio:              e.cfg.BTSwarmSeedDropRatio,
		SpeedDropRatio:             e.cfg.BTSwarmSpeedDropRatio,
		IncomingConnectivityNote:   "Incoming peers may not reach this client unless TCP/UDP torrent port is forwarded or UPnP succeeds.",
		Torrents:                   make([]models.BTTorrentHealth, 0, len(mts)),
	}
	e.mu.RUnlock()
	if pc, ok := e.pieceCompletion.(interface{ Status() completion.Stats }); ok {
		status := pc.Status()
		health.PieceCompletionError = status.Error
		health.PieceCompletionDropped = status.Dropped
		health.PieceCompletionPending = status.Pending
	}

	for _, mt := range mts {
		stats := mt.t.Stats()

		mt.mu.Lock()
		maxEstablishedConns := mt.normalMaxEstablishedConns
		if maxEstablishedConns == 0 {
			maxEstablishedConns = e.cfg.BTEstablishedConns
		}
		if !mt.boostUntil.IsZero() {
			maxEstablishedConns = e.cfg.BTSwarmBoostConns
		}
		mtTrackerStatus := mt.trackerStatus
		mtTrackerError := mt.trackerError
		mtDegraded := mt.degraded
		mtLastSwarmRefreshAt := mt.lastSwarmRefreshAt
		mtLastSwarmRefreshReason := mt.lastSwarmRefreshReason
		mtLastPeerRefreshAt := mt.lastPeerRefreshAt
		mtLastPeerRefreshReason := mt.lastPeerRefreshReason
		mtLastHealthyAt := mt.lastHealthyAt
		mtBoostUntil := mt.boostUntil
		mtDownloadSpeed := mt.downloadSpeed
		mtUploadSpeed := mt.uploadSpeed
		mtPeakDownloadSpeed := mt.peakDownloadSpeed
		mtPeakUpdatedAt := mt.peakUpdatedAt
		mtState := mt.state
		mt.mu.Unlock()

		info := mt.t.Info()
		trackerTiers, trackerURLs := trackerCounts(mt)
		activeStreams := e.streamManager.ActiveStreamsForTorrent(mt.t.InfoHash().HexString())
		var storageIO *models.StorageIOTimings
		if io, ok := mt.t.StorageIOTimings(); ok {
			storageIO = &models.StorageIOTimings{
				ManagedPayload: io.ManagedPayload, FileCacheHits: io.FileCacheHits, FileCacheMisses: io.FileCacheMisses,
				FileCacheEvictions: io.FileCacheEvictions, FileCacheOpen: io.FileCacheOpen,
				CompletionSyncCount: io.CompletionSyncCount, CompletionSyncErrors: io.CompletionSyncErrors,
				CompletionSyncActive: io.CompletionSyncActive, CompletionSyncNS: io.CompletionSyncNS,
				CompletionSyncMaxNS: io.CompletionSyncMaxNS, WriteLeaseWaitCount: io.WriteLeaseWaitCount,
				WriteLeaseWaitActive: io.WriteLeaseWaitActive, WriteLeaseWaitNS: io.WriteLeaseWaitNS,
				WriteLeaseWaitMaxNS: io.WriteLeaseWaitMaxNS,
				WriteLeaseHold:      storageIOStage(io.WriteLeaseHold), WritePrepare: storageIOStage(io.WritePrepare),
				WriteGuard: storageIOStage(io.WriteGuard), WriteOpen: storageIOStage(io.WriteOpen),
				WriteStat: storageIOStage(io.WriteStat), WritePayload: storageIOStage(io.WritePayload),
				WriteAccount: storageIOStage(io.WriteAccount), WriteClose: storageIOStage(io.WriteClose),
				WriteOpenGuard: storageIOStage(io.WriteOpenGuard), WriteOpenPath: storageIOStage(io.WriteOpenPath),
				WriteOpenMkdir: storageIOStage(io.WriteOpenMkdir), WriteOpenDescriptor: storageIOStage(io.WriteOpenDescriptor),
				WriteOpenIdentity: storageIOStage(io.WriteOpenIdentity),
			}
		}

		health.Torrents = append(health.Torrents, models.BTTorrentHealth{
			StorageIO:             storageIO,
			Hash:                  mt.hash,
			Name:                  mt.t.Name(),
			State:                 mtState,
			Known:                 stats.TotalPeers,
			Connected:             stats.ActivePeers,
			Pending:               stats.PendingPeers,
			HalfOpen:              stats.HalfOpenPeers,
			Seeds:                 stats.ConnectedSeeders,
			BytesRead:             stats.BytesRead.Int64(),
			BytesReadData:         stats.BytesReadData.Int64(),
			BytesReadUsefulData:   stats.BytesReadUsefulData.Int64(),
			BytesWritten:          stats.BytesWritten.Int64(),
			BytesWrittenData:      stats.BytesWrittenData.Int64(),
			ChunksRead:            stats.ChunksRead.Int64(),
			ChunksReadUseful:      stats.ChunksReadUseful.Int64(),
			ChunksReadWasted:      stats.ChunksReadWasted.Int64(),
			PiecesDirtiedGood:     stats.PiecesDirtiedGood.Int64(),
			PiecesDirtiedBad:      stats.PiecesDirtiedBad.Int64(),
			DownloadSpeed:         mtDownloadSpeed,
			UploadSpeed:           mtUploadSpeed,
			WasteRatio:            wasteRatio(stats.ChunksReadUseful.Int64(), stats.ChunksReadWasted.Int64()),
			TrackerTiersCount:     trackerTiers,
			TrackerURLsCount:      trackerURLs,
			MetadataReady:         info != nil,
			TrackerStatus:         mtTrackerStatus,
			TrackerError:          mtTrackerError,
			Degraded:              mtDegraded,
			PeakDownloadSpeed:     mtPeakDownloadSpeed,
			PeakUpdatedAt:         formatBTHealthTime(mtPeakUpdatedAt),
			LastRefreshAt:         formatBTHealthTime(mtLastSwarmRefreshAt),
			LastRefreshReason:     mtLastSwarmRefreshReason,
			LastPeerRefreshAt:     formatBTHealthTime(mtLastPeerRefreshAt),
			LastPeerRefreshReason: mtLastPeerRefreshReason,
			LastHealthyAt:         formatBTHealthTime(mtLastHealthyAt),
			BoostedUntil:          formatBTHealthTime(mtBoostUntil),
			MaxEstablishedConns:   maxEstablishedConns,
			ActiveStreams:         activeStreams,
		})
	}

	return health
}

func formatBTHealthTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func storageIOStage(s storage.IOStageTimings) models.StorageIOStageTimings {
	return models.StorageIOStageTimings{Count: s.Count, Errors: s.Errors, Active: s.Active, NS: s.NS, MaxNS: s.MaxNS}
}

func normalizedBTClientProfile(profile string) string {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "default":
		return "default"
	default:
		return "qbittorrent"
	}
}

func normalizedRetrackersMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "off", "replace":
		return strings.ToLower(strings.TrimSpace(mode))
	default:
		return "append"
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func wasteRatio(useful, wasted int64) float64 {
	total := useful + wasted
	if total <= 0 {
		return 0
	}
	return float64(wasted) / float64(total)
}

func trackerCounts(mt *ManagedTorrent) (int, int) {
	if mt == nil || mt.t == nil {
		return 0, 0
	}
	mi := mt.t.Metainfo()
	list := mi.UpvertedAnnounceList()
	urls := 0
	for _, tier := range list {
		urls += len(tier)
	}
	return len(list), urls
}

func (e *Engine) mapManagedTorrent(mt *ManagedTorrent) *models.Torrent {
	observedAt := time.Now().UnixNano()
	t := mt.t
	info := t.Info()
	size := int64(0)
	if info != nil {
		size = info.TotalLength()
	}

	var prefix func(int64) int64
	if info != nil {
		prefix = verifiedPrefix(t.PieceStateRuns(), info.PieceLength)
	}

	stats := t.Stats()

	mt.mu.Lock()
	peerSummary := models.PeerSummary{
		Known:         stats.TotalPeers,
		Connected:     stats.ActivePeers,
		Pending:       stats.PendingPeers,
		HalfOpen:      stats.HalfOpenPeers,
		Seeds:         stats.ConnectedSeeders,
		MetadataReady: info != nil,
		TrackerStatus: mt.trackerStatus,
		TrackerError:  mt.trackerError,
		DHTStatus:     e.dhtStatus(),
	}
	e.logPeerSummaryLocked(t.InfoHash().HexString(), peerSummary, mt)

	model := &models.Torrent{
		Control:        &models.TorrentControl{RecordID: mt.runtimeFence.RecordID, RuntimeAttachment: mt.runtimeFence.Attachment, AppliedRevision: mt.appliedIntentRevision},
		Hash:           mt.hash,
		Name:           t.Name(),
		Size:           size,
		ProgressSource: "unknown",
		MetadataKnown:  info != nil,
		State:          mt.state,
		Error:          mt.err,
		DownloadBlock:  mt.downloadBlock,

		DownloadSpeed:     mt.downloadSpeed,
		UploadSpeed:       mt.uploadSpeed,
		Peers:             peerSummary.Connected,
		Seeds:             peerSummary.Seeds,
		PeerSummary:       peerSummary,
		SourceURI:         mt.sourceURI,
		QueuePosition:     mt.queuePosition,
		SelectionComplete: mt.selectionComplete,
		Activity:          models.TorrentActivity{Verifying: mt.rechecking},
	}
	if mt.intentApplyFailure != nil {
		failure := *mt.intentApplyFailure
		model.Control.ApplyFailure = &failure
	}
	if mt.recheckOperation != nil {
		operation := *mt.recheckOperation
		model.Recheck = &operation
	}
	mt.mu.Unlock()
	if e.streamManager != nil {
		a := e.streamManager.activityForTorrent(mt)
		a.Verifying = model.Activity.Verifying
		model.Activity = a
	}

	if info != nil {
		model.MetadataFormat = "v1"
		if info.HasV2() {
			model.MetadataFormat = "v2"
			if info.HasV1() {
				model.MetadataFormat = "hybrid"
			}
		}
		model.PieceLength = info.PieceLength
		model.NumPieces = info.NumPieces()
		model.ProgressSource = "verified"
		model.ProgressObservedAt = observedAt
		observations := t.FileObservations()
		resume := t.FileResumeObservation()
		model.Resume = &models.ResumeProgress{Mode: resume.Mode, Status: resume.Status, Reason: resume.Reason, CheckpointGeneration: resume.Generation, CheckpointAtMS: resume.CheckpointAtMS}
		if model.Resume.Mode == "" {
			model.Resume.Mode = e.FastResumeMode()
		}
		if model.Resume.Status == "" {
			model.Resume.Status, model.Resume.Reason = "fallback", "unsupported_storage"
		}
		verification := &models.VerificationProgress{Attachment: model.Control.RuntimeAttachment, Status: "completed", TotalBytes: size}
		if model.Recheck != nil {
			verification.Operation = model.Recheck.ID
		}
		model.Verification = verification

		mt.mu.Lock()
		checking := mt.rechecking || mt.initializing
		uncertain := mt.verificationUncertain
		for i, file := range t.Files() {
			prio := models.PriorityNormal
			if p, ok := mt.filePriorities[i]; ok {
				prio = p
			}
			downloaded := prefix(file.Offset()+file.Length()) - prefix(file.Offset())
			if checking || uncertain {
				downloaded = 0
			}
			model.Downloaded += downloaded
			fi := file.FileInfo()
			status := &models.FileStatus{Presence: "unknown", Verification: "unknown", EffectivePriority: "none", ReadReason: "verification_required", VerificationTotal: file.Length()}
			if i < len(observations) {
				o := observations[i]
				if !checking && !uncertain {
					model.Resume.RestoredBytes += o.RestoredBytes
					model.Resume.FreshlyVerifiedBytes += o.FreshlyVerifiedBytes
				}
				status.VerifiedBytes, status.CheckedBytes = o.VerifiedBytes, o.CheckedBytes
				status.SkippedMissingBytes = o.SkippedMissingBytes
				status.EffectivePriority = observedPriority(o.EffectivePriority)
				status.Verification = "completed"
				if o.QueuedPieces > 0 {
					status.Verification = "queued"
				}
				if o.ActivePieces > 0 {
					status.Verification = "running"
				}
				status.DirectReadReady = o.DirectReadReady && !checking && !uncertain && file.Length() > 0
				if status.DirectReadReady {
					status.ReadReason = ""
				} else if status.Verification == "completed" && !checking && !uncertain {
					status.ReadReason = "incomplete"
				}
				verification.CheckedBytes += o.CheckedBytes
				verification.SkippedMissingBytes += o.SkippedMissingBytes
				// Piece counts below are collected once from runs, not duplicated
				// for shared v1 boundaries.
			}
			if uncertain {
				status.VerifiedBytes = 0
				status.Verification = "unknown"
			}
			if checking && status.Verification == "completed" && status.CheckedBytes == 0 && file.Length() > 0 {
				status.Verification = "queued"
			}
			if model.Recheck != nil && (model.Recheck.Status == "failed" || model.Recheck.Status == "cancelled") {
				status.Verification = model.Recheck.Status
			}
			model.Files = append(model.Files, &models.File{
				MetadataPath: payload.Path(info, &fi),
				Status:       status,
				Index:        i,
				Path:         file.DisplayPath(),
				Size:         file.Length(),
				Offset:       file.Offset(),
				OffsetKnown:  true,
				Downloaded:   downloaded,
				Priority:     prio,
				IsMedia:      isMediaFile(file.DisplayPath()),
			})
		}
		if checking {
			model.ProgressSource = "checking"
			model.ProgressObservedAt = 0
		} else if uncertain {
			model.ProgressSource = "unknown"
			model.ProgressObservedAt = 0
		}
		mt.mu.Unlock()
		for _, run := range t.PieceStateRuns() {
			if run.QueuedForHash || run.WaitingForHashBudget {
				verification.QueuedPieces += run.Length
			}
			if run.Hashing && !run.WaitingForHashBudget || run.Marking {
				verification.ActivePieces += run.Length
			}
		}
		if verification.QueuedPieces > 0 || checking {
			verification.Status = "queued"
		}
		if verification.ActivePieces > 0 {
			verification.Status = "running"
		}
		if uncertain {
			verification.Status = "unknown"
		}
		if model.Recheck != nil && (model.Recheck.Status == "failed" || model.Recheck.Status == "cancelled") {
			verification.Status = model.Recheck.Status
		}
		mt.mu.Lock()
		if mt.verificationSampleOperation != verification.Operation || verification.CheckedBytes < mt.verificationSampleBytes {
			mt.verificationProgressAtMS, mt.verificationSampleBytes = 0, 0
		}
		if verification.CheckedBytes > mt.verificationSampleBytes {
			mt.verificationProgressAtMS = observedAt / 1e6
		}
		mt.verificationSampleOperation, mt.verificationSampleBytes = verification.Operation, verification.CheckedBytes
		verification.ProgressAtMS = mt.verificationProgressAtMS
		mt.mu.Unlock()
		model.Activity.Verifying = model.Activity.Verifying || verification.QueuedPieces > 0 || verification.ActivePieces > 0
	}
	model.Progress = models.ProgressPercent(model.Downloaded, model.Size)

	return model
}

func observedPriority(p torrent.PiecePriority) string {
	switch p {
	case torrent.PiecePriorityNow:
		return "now"
	case torrent.PiecePriorityReadahead:
		return "readahead"
	case torrent.PiecePriorityNext:
		return "next"
	case torrent.PiecePriorityHigh:
		return "high"
	case torrent.PiecePriorityNormal:
		return "normal"
	default:
		return "none"
	}
}

func isMediaFile(filePath string) bool {
	switch strings.ToLower(filepath.Ext(filePath)) {
	case ".mp4", ".mkv", ".avi", ".mov", ".m4v", ".webm", ".ts":
		return true
	default:
		return false
	}
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (e *Engine) sampleSpeedsLocked(mt *ManagedTorrent, stats torrent.TorrentStats, now time.Time) {
	if !mt.lastStatsAt.IsZero() && !now.After(mt.lastStatsAt) {
		return
	}
	rawReadBytes := stats.BytesRead.Int64()
	dataReadBytes := stats.BytesReadData.Int64()
	readBytes := stats.BytesReadUsefulData.Int64()
	writtenBytes := stats.PeerConns.BytesWrittenData.Int64() + stats.WebSeeds.BytesWrittenData.Int64()

	if !mt.lastStatsAt.IsZero() {
		elapsed := now.Sub(mt.lastStatsAt).Seconds()
		if elapsed > 0 {
			rawReadDelta := rawReadBytes - mt.lastRawReadBytes
			dataReadDelta := dataReadBytes - mt.lastDataReadBytes
			readDelta := readBytes - mt.lastReadBytes
			writtenDelta := writtenBytes - mt.lastWrittenBytes
			if rawReadDelta < 0 {
				rawReadDelta = 0
			}
			if dataReadDelta < 0 {
				dataReadDelta = 0
			}
			if readDelta < 0 {
				readDelta = 0
			}
			if writtenDelta < 0 {
				writtenDelta = 0
			}
			mt.rawDownloadSpeed = int64(float64(rawReadDelta) / elapsed)
			mt.dataDownloadSpeed = int64(float64(dataReadDelta) / elapsed)
			mt.downloadSpeed = int64(float64(readDelta) / elapsed)
			mt.uploadSpeed = int64(float64(writtenDelta) / elapsed)
		}
	}

	mt.lastStatsAt = now
	mt.lastRawReadBytes = rawReadBytes
	mt.lastDataReadBytes = dataReadBytes
	mt.lastReadBytes = readBytes
	mt.lastWrittenBytes = writtenBytes
}

func (e *Engine) logPeerSummaryLocked(hash string, summary models.PeerSummary, mt *ManagedTorrent) {
	if !logging.IsDebugEnabled() {
		return
	}

	now := time.Now()
	changed := summary != mt.lastPeerSummary
	periodic := mt.lastPeerLog.IsZero() || now.Sub(mt.lastPeerLog) >= 30*time.Second
	if !changed && !periodic {
		return
	}

	logging.Debugf("peer summary hash=%s known=%d connected=%d pending=%d half_open=%d seeds=%d metadata_ready=%t tracker_status=%s dht_status=%s download_speed=%d upload_speed=%d",
		hash,
		summary.Known,
		summary.Connected,
		summary.Pending,
		summary.HalfOpen,
		summary.Seeds,
		summary.MetadataReady,
		summary.TrackerStatus,
		summary.DHTStatus,
		mt.downloadSpeed,
		mt.uploadSpeed,
	)
	mt.lastPeerSummary = summary
	mt.lastPeerLog = now
}

func (e *Engine) dhtStatus() string {
	if e.ctx != nil && e.ctx.Err() != nil {
		return "disabled"
	}
	if e.client == nil {
		return "disabled"
	}
	if len(e.client.DhtServers()) == 0 {
		return "disabled"
	}
	return "enabled"
}
