package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"torrent-stream-hub/internal/logging"
	"torrent-stream-hub/internal/models"

	"github.com/anacrolix/torrent"
)

const (
	DebounceDelay              = 10 * time.Second
	defaultStreamHeadPrebuffer = 20 << 20
	defaultStreamTailPieces    = 4
	defaultStreamBehindPieces  = 2
	defaultStreamSlidingWindow = 20 << 20
)

type FileKey struct {
	Hash  string
	Index int
}

type StreamState struct {
	ActiveStreams  int
	ActivePreloads int
	DebounceTimer  *time.Timer
	Sessions       map[int64]StreamSession
	NextSessionID  int64
	AppliedRanges  []streamPieceRange
	// Track when it's fully downloaded so we can remove sequential mode
	FullyDownloaded bool
}

type StreamOptions struct {
	RangeStart  int64
	RangeEnd    int64
	HasRange    bool
	IsHEAD      bool
	SkipQoS     bool
	WindowBytes int64
}

type StreamSession struct {
	ID      int64
	Options StreamOptions
	Preload bool
}

type streamPieceRange struct {
	Begin int
	End   int
}

type streamFileBounds struct {
	Offset      int64
	Length      int64
	BeginPiece  int
	EndPiece    int
	DisplayPath string
}

type StreamManager struct {
	mu     sync.Mutex
	engine *Engine
	states map[FileKey]*StreamState
}

func NewStreamManager(e *Engine) *StreamManager {
	return &StreamManager{
		engine: e,
		states: make(map[FileKey]*StreamState),
	}
}

func (sm *StreamManager) ActiveStreamsForTorrent(hash string) int {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	active := 0
	for key, state := range sm.states {
		if key.Hash != hash || state == nil {
			continue
		}
		if state.ActiveStreams > 0 {
			active += state.ActiveStreams
			continue
		}
		if state.DebounceTimer != nil {
			active++
		}
	}
	return active
}

func (sm *StreamManager) ActiveStreamsTotal() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	active := 0
	for _, state := range sm.states {
		if state == nil {
			continue
		}
		if state.ActiveStreams > 0 {
			active += state.ActiveStreams
			continue
		}
		if state.DebounceTimer != nil {
			active++
		}
	}
	return active
}

// AddStream increments the reference count for a file and applies a range-aware QoS overlay.
func (sm *StreamManager) AddStream(ctx context.Context, hash string, fileIndex int, opts StreamOptions) error {
	if opts.IsHEAD || opts.SkipQoS {
		logging.Debugf("stream QoS skipped hash=%s file_index=%d head=%t skip=%t", hash, fileIndex, opts.IsHEAD, opts.SkipQoS)
		return nil
	}

	sessionID, err := sm.addSession(hash, fileIndex, opts, false)
	if err != nil {
		return err
	}

	if err := sm.refreshStreamOverlay(hash, fileIndex); err != nil {
		sm.rollbackSession(hash, fileIndex, sessionID)
		logging.Warnf("failed to apply stream QoS hash=%s file_index=%d: %v", hash, fileIndex, err)
		return err
	}

	// Watch for context cancellation to remove the stream.
	go func() {
		<-ctx.Done()
		sm.removeSession(hash, fileIndex, sessionID, true)
	}()

	return nil
}

func (sm *StreamManager) AddPreload(ctx context.Context, hash string, fileIndex int, opts StreamOptions) (func(), error) {
	if opts.SkipQoS || opts.IsHEAD {
		return func() {}, nil
	}

	sessionID, err := sm.addSession(hash, fileIndex, opts, true)
	if err != nil {
		return nil, err
	}
	if err := sm.refreshStreamOverlay(hash, fileIndex); err != nil {
		sm.rollbackSession(hash, fileIndex, sessionID)
		return nil, err
	}

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			sm.removeSession(hash, fileIndex, sessionID, false)
		})
	}
	return cleanup, nil
}

func (sm *StreamManager) addSession(hash string, fileIndex int, opts StreamOptions, preload bool) (int64, error) {
	sm.mu.Lock()

	key := FileKey{Hash: hash, Index: fileIndex}
	state, exists := sm.states[key]
	logging.Infof("stream add requested hash=%s file_index=%d existing=%t", hash, fileIndex, exists)

	if !exists {
		state = &StreamState{
			Sessions: make(map[int64]StreamSession),
		}
		sm.states[key] = state
	} else if state.Sessions == nil {
		state.Sessions = make(map[int64]StreamSession)
	}

	// Cancel existing debounce timer if any (e.g. user seeked within 10s)
	if state.DebounceTimer != nil {
		state.DebounceTimer.Stop()
		state.DebounceTimer = nil
		logging.Debugf("stream debounce cancelled hash=%s file_index=%d", hash, fileIndex)
	}

	state.NextSessionID++
	sessionID := state.NextSessionID
	state.Sessions[sessionID] = StreamSession{ID: sessionID, Options: opts, Preload: preload}
	if preload {
		state.ActivePreloads++
		logging.Debugf("stream preload reference incremented hash=%s file_index=%d active_preloads=%d", hash, fileIndex, state.ActivePreloads)
	} else {
		state.ActiveStreams++
		logging.Debugf("stream reference incremented hash=%s file_index=%d active=%d range_start=%d has_range=%t", hash, fileIndex, state.ActiveStreams, opts.RangeStart, opts.HasRange)
	}
	sm.mu.Unlock()

	return sessionID, nil
}

func (sm *StreamManager) rollbackSession(hash string, fileIndex int, sessionID int64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	key := FileKey{Hash: hash, Index: fileIndex}
	state, ok := sm.states[key]
	if !ok || state == nil {
		return
	}
	session, ok := state.Sessions[sessionID]
	if !ok {
		return
	}
	delete(state.Sessions, sessionID)
	if session.Preload {
		state.ActivePreloads--
	} else {
		state.ActiveStreams--
	}
	if state.ActiveStreams < 0 {
		state.ActiveStreams = 0
	}
	if state.ActivePreloads < 0 {
		state.ActivePreloads = 0
	}
	if state.ActiveStreams == 0 && state.ActivePreloads == 0 && len(state.AppliedRanges) == 0 {
		delete(sm.states, key)
	}
}

// RemoveStream keeps the legacy behavior used by tests and removes one active stream session.
func (sm *StreamManager) RemoveStream(hash string, fileIndex int) {
	sm.mu.Lock()
	key := FileKey{Hash: hash, Index: fileIndex}
	state, exists := sm.states[key]
	if !exists || state == nil {
		sm.mu.Unlock()
		return
	}
	var sessionID int64
	for id, session := range state.Sessions {
		if !session.Preload {
			sessionID = id
			break
		}
	}
	if sessionID == 0 && len(state.Sessions) == 0 && state.ActiveStreams > 0 {
		state.ActiveStreams--
		logging.Debugf("stream reference decremented hash=%s file_index=%d active=%d", hash, fileIndex, state.ActiveStreams)
		shouldDebounce := state.ActiveStreams <= 0 && state.ActivePreloads == 0
		state.ActiveStreams = maxInt(0, state.ActiveStreams)
		sm.mu.Unlock()
		if shouldDebounce {
			sm.scheduleDebounce(hash, fileIndex)
		}
		return
	}
	sm.mu.Unlock()

	if sessionID != 0 {
		sm.removeSession(hash, fileIndex, sessionID, true)
	}
}

func (sm *StreamManager) removeSession(hash string, fileIndex int, sessionID int64, debounce bool) {
	sm.mu.Lock()

	key := FileKey{Hash: hash, Index: fileIndex}
	state, exists := sm.states[key]
	if !exists || state == nil {
		sm.mu.Unlock()
		return
	}

	session, sessionExists := state.Sessions[sessionID]
	if sessionExists {
		delete(state.Sessions, sessionID)
		if session.Preload {
			state.ActivePreloads--
			logging.Debugf("stream preload reference decremented hash=%s file_index=%d active_preloads=%d", hash, fileIndex, state.ActivePreloads)
		} else {
			state.ActiveStreams--
			logging.Debugf("stream reference decremented hash=%s file_index=%d active=%d", hash, fileIndex, state.ActiveStreams)
		}
	}
	if state.ActiveStreams < 0 {
		state.ActiveStreams = 0
	}
	if state.ActivePreloads < 0 {
		state.ActivePreloads = 0
	}

	activeStreams := state.ActiveStreams
	activePreloads := state.ActivePreloads
	sm.mu.Unlock()

	if activeStreams > 0 || activePreloads > 0 {
		if err := sm.refreshStreamOverlay(hash, fileIndex); err != nil {
			logging.Warnf("failed to refresh stream QoS hash=%s file_index=%d: %v", hash, fileIndex, err)
		}
		return
	}

	if debounce {
		sm.scheduleDebounce(hash, fileIndex)
		return
	}
	sm.clearStreamOverlay(hash, fileIndex)
}

func (sm *StreamManager) scheduleDebounce(hash string, fileIndex int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	key := FileKey{Hash: hash, Index: fileIndex}
	state, exists := sm.states[key]
	if !exists || state == nil {
		return
	}
	if state.DebounceTimer != nil {
		state.DebounceTimer.Stop()
	}

	state.DebounceTimer = time.AfterFunc(DebounceDelay, func() {
		sm.mu.Lock()

		// Verify if it's still inactive after delay.
		st, ok := sm.states[key]
		if ok && st.ActiveStreams == 0 && st.ActivePreloads == 0 {
			sm.mu.Unlock()
			sm.clearStreamOverlay(hash, fileIndex)
			logging.Infof("stream debounce elapsed hash=%s file_index=%d qos_cleared=%t", hash, fileIndex, true)
			return
		}
		sm.mu.Unlock()
	})
	logging.Debugf("stream debounce scheduled hash=%s file_index=%d delay=%s", hash, fileIndex, DebounceDelay)
}

func (sm *StreamManager) refreshStreamOverlay(hash string, fileIndex int) error {
	mt, file, err := sm.streamFile(hash, fileIndex)
	if err != nil {
		return err
	}

	key := FileKey{Hash: hash, Index: fileIndex}
	sm.mu.Lock()
	state, ok := sm.states[key]
	if !ok || state == nil {
		sm.mu.Unlock()
		return nil
	}
	sessions := make([]StreamSession, 0, len(state.Sessions))
	for _, session := range state.Sessions {
		sessions = append(sessions, session)
	}
	oldRanges := cloneStreamPieceRanges(state.AppliedRanges)
	sm.mu.Unlock()

	var newRanges []streamPieceRange
	fullyDownloaded := file.BytesCompleted() == file.Length()
	if !fullyDownloaded {
		for _, session := range sessions {
			newRanges = append(newRanges, sm.computeStreamPieceRanges(file, session.Options)...)
		}
		newRanges = mergeStreamPieceRanges(newRanges)
	}

	if fullyDownloaded {
		logging.Debugf("stream QoS skipped for completed file hash=%s file_index=%d", hash, fileIndex)
	}

	if err := sm.applyOverlayDiff(mt, key, oldRanges, newRanges); err != nil {
		return err
	}

	sm.mu.Lock()
	if st, ok := sm.states[key]; ok && st != nil {
		st.AppliedRanges = cloneStreamPieceRanges(newRanges)
		st.FullyDownloaded = fullyDownloaded
	}
	sm.mu.Unlock()

	logging.Debugf("stream QoS refreshed hash=%s file_index=%d sessions=%d ranges=%v", hash, fileIndex, len(sessions), newRanges)
	return nil
}

func (sm *StreamManager) clearStreamOverlay(hash string, fileIndex int) {
	key := FileKey{Hash: hash, Index: fileIndex}
	sm.mu.Lock()
	state, ok := sm.states[key]
	if !ok || state == nil {
		sm.mu.Unlock()
		return
	}
	oldRanges := cloneStreamPieceRanges(state.AppliedRanges)
	delete(sm.states, key)
	sm.mu.Unlock()

	mt, _, err := sm.streamFile(hash, fileIndex)
	if err != nil {
		logging.Warnf("failed to clear stream QoS hash=%s file_index=%d: %v", hash, fileIndex, err)
		return
	}

	if err := sm.applyOverlayDiff(mt, key, oldRanges, nil); err != nil {
		logging.Warnf("failed to clear stream QoS hash=%s file_index=%d: %v", hash, fileIndex, err)
	}
	sm.restoreBasePriorities(mt)
}

func (sm *StreamManager) streamFile(hash string, fileIndex int) (*ManagedTorrent, *torrent.File, error) {
	sm.engine.mu.RLock()
	mt, ok := sm.engine.managedTorrents[hash]
	sm.engine.mu.RUnlock()

	if !ok {
		logging.Debugf("stream QoS requested for missing torrent hash=%s file_index=%d", hash, fileIndex)
		return nil, nil, TorrentNotFoundError{Hash: hash}
	}
	if mt.t.Info() == nil {
		logging.Debugf("stream QoS requested before metadata hash=%s file_index=%d", hash, fileIndex)
		return nil, nil, fmt.Errorf("torrent metadata is not available yet")
	}

	files := mt.t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		logging.Debugf("stream QoS file index out of bounds hash=%s file_index=%d files=%d", hash, fileIndex, len(files))
		return nil, nil, fmt.Errorf("file index out of bounds: %d", fileIndex)
	}

	return mt, files[fileIndex], nil
}

func (sm *StreamManager) applyOverlayDiff(mt *ManagedTorrent, key FileKey, oldRanges, newRanges []streamPieceRange) error {
	protectedRanges := sm.aggregateTorrentRanges(key.Hash, key, newRanges)
	for _, r := range oldRanges {
		for piece := r.Begin; piece < r.End; piece++ {
			if streamRangesContainPiece(protectedRanges, piece) {
				continue
			}
			if piece >= 0 && piece < mt.t.NumPieces() {
				mt.t.Piece(piece).SetPriority(torrent.PiecePriorityNone)
			}
		}
	}

	for _, r := range newRanges {
		for piece := r.Begin; piece < r.End; piece++ {
			if piece >= 0 && piece < mt.t.NumPieces() {
				mt.t.Piece(piece).SetPriority(torrent.PiecePriorityHigh)
			}
		}
	}
	return nil
}

func (sm *StreamManager) restoreBasePriorities(mt *ManagedTorrent) {
	mt.mu.Lock()
	state := mt.state
	mt.mu.Unlock()

	if state == models.StatePaused || state == models.StateError || state == models.StateMissingFiles || state == models.StateDiskFull {
		logging.Debugf("torrent is in inactive state %s, restoring inactive priorities hash=%s", state, mt.t.InfoHash().HexString())
		for _, f := range mt.t.Files() {
			f.SetPriority(torrent.PiecePriorityNone)
		}
		mt.t.CancelPieces(0, mt.t.NumPieces())
		return
	}
	sm.engine.applyFilePrioritiesAndDownload(mt)
}

func (sm *StreamManager) aggregateTorrentRanges(hash string, replaceKey FileKey, replacement []streamPieceRange) []streamPieceRange {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	ranges := cloneStreamPieceRanges(replacement)
	for key, state := range sm.states {
		if key.Hash != hash || key == replaceKey || state == nil {
			continue
		}
		ranges = append(ranges, state.AppliedRanges...)
	}
	return mergeStreamPieceRanges(ranges)
}

func (sm *StreamManager) computeStreamPieceRanges(file *torrent.File, opts StreamOptions) []streamPieceRange {
	info := file.Torrent().Info()
	if info == nil {
		return nil
	}
	bounds := streamFileBounds{
		Offset:      file.Offset(),
		Length:      file.Length(),
		BeginPiece:  file.BeginPieceIndex(),
		EndPiece:    file.EndPieceIndex(),
		DisplayPath: file.DisplayPath(),
	}
	return computeStreamPieceRangesForBounds(bounds, info.PieceLength, sm.engine.cfg.StreamCacheSize, opts)
}

func computeStreamPieceRangesForBounds(bounds streamFileBounds, pieceLength, streamCacheSize int64, opts StreamOptions) []streamPieceRange {
	if bounds.Length <= 0 || pieceLength <= 0 || bounds.BeginPiece >= bounds.EndPiece {
		return nil
	}

	windowBytes := opts.WindowBytes
	if windowBytes <= 0 {
		windowBytes = streamCacheSize
	}
	if windowBytes <= 0 {
		windowBytes = defaultStreamSlidingWindow
	}

	headBytes := minPositiveInt64(defaultStreamHeadPrebuffer, bounds.Length)
	if opts.WindowBytes > 0 {
		headBytes = minPositiveInt64(opts.WindowBytes, bounds.Length)
	}

	ranges := make([]streamPieceRange, 0, 4)
	if r, ok := streamByteRegionPieces(bounds, pieceLength, 0, headBytes); ok {
		ranges = append(ranges, r)
	}

	if streamFileHasTailMetadata(bounds.DisplayPath) {
		begin := bounds.EndPiece - defaultStreamTailPieces
		if begin < bounds.BeginPiece {
			begin = bounds.BeginPiece
		}
		ranges = append(ranges, streamPieceRange{Begin: begin, End: bounds.EndPiece})
	}

	playhead := opts.RangeStart
	if playhead < 0 {
		playhead = 0
	}
	if playhead >= bounds.Length {
		return mergeStreamPieceRanges(ranges)
	}
	remaining := bounds.Length - playhead
	if windowBytes > remaining {
		windowBytes = remaining
	}
	if r, ok := streamByteRegionPieces(bounds, pieceLength, playhead, windowBytes); ok {
		r.Begin -= defaultStreamBehindPieces
		if r.Begin < bounds.BeginPiece {
			r.Begin = bounds.BeginPiece
		}
		ranges = append(ranges, r)
	}

	return mergeStreamPieceRanges(ranges)
}

func streamByteRegionPieces(bounds streamFileBounds, pieceLength, relOffset, size int64) (streamPieceRange, bool) {
	if relOffset < 0 {
		size += relOffset
		relOffset = 0
	}
	if size <= 0 || relOffset >= bounds.Length {
		return streamPieceRange{}, false
	}
	if relOffset+size > bounds.Length {
		size = bounds.Length - relOffset
	}
	absStart := bounds.Offset + relOffset
	absEnd := absStart + size
	begin := int(absStart / pieceLength)
	end := int((absEnd + pieceLength - 1) / pieceLength)
	if begin < bounds.BeginPiece {
		begin = bounds.BeginPiece
	}
	if end > bounds.EndPiece {
		end = bounds.EndPiece
	}
	if begin >= end {
		return streamPieceRange{}, false
	}
	return streamPieceRange{Begin: begin, End: end}, true
}

func mergeStreamPieceRanges(ranges []streamPieceRange) []streamPieceRange {
	if len(ranges) == 0 {
		return nil
	}
	items := make([]streamPieceRange, 0, len(ranges))
	for _, r := range ranges {
		if r.Begin < r.End {
			items = append(items, r)
		}
	}
	if len(items) == 0 {
		return nil
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Begin == items[j].Begin {
			return items[i].End < items[j].End
		}
		return items[i].Begin < items[j].Begin
	})
	merged := []streamPieceRange{items[0]}
	for _, r := range items[1:] {
		last := &merged[len(merged)-1]
		if r.Begin <= last.End {
			if r.End > last.End {
				last.End = r.End
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

func streamRangesContainPiece(ranges []streamPieceRange, piece int) bool {
	for _, r := range ranges {
		if piece >= r.Begin && piece < r.End {
			return true
		}
	}
	return false
}

func cloneStreamPieceRanges(ranges []streamPieceRange) []streamPieceRange {
	if len(ranges) == 0 {
		return nil
	}
	clone := make([]streamPieceRange, len(ranges))
	copy(clone, ranges)
	return clone
}

func streamFileHasTailMetadata(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".mov", ".m4v":
		return true
	default:
		return false
	}
}

func minPositiveInt64(a, b int64) int64 {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
