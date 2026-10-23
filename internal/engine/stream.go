package engine

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"torrent-stream-hub/internal/logging"

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
	Torrent        *ManagedTorrent
	TimerVersion   uint64
	ActiveStreams  int
	ActivePreloads int
	DebounceTimer  *time.Timer
	Sessions       map[int64]StreamSession
	AppliedRanges  []streamPieceRange
	// Track when it's fully downloaded so we can remove sequential mode
	FullyDownloaded bool
}

type StreamOptions struct {
	RangeStart     int64
	RangeEnd       int64
	HasRange       bool
	IsHEAD         bool
	SkipQoS        bool
	WindowBytes    int64
	AccountingOnly bool
}

type StreamSession struct {
	Done           chan struct{}
	ID             int64
	Options        StreamOptions
	Preload        bool
	AccountingOnly bool
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

type directStreamHandle struct {
	id         int64
	torrent    *ManagedTorrent
	closer     io.Closer
	cancelChan chan struct{}
}

type StreamManager struct {
	mu            sync.Mutex
	engine        *Engine
	states        map[FileKey]*StreamState
	directStreams map[int64]*directStreamHandle
	nextDirectID  int64
	nextSessionID int64
	closed        bool
}

func NewStreamManager(e *Engine) *StreamManager {
	return &StreamManager{
		engine:        e,
		states:        make(map[FileKey]*StreamState),
		directStreams: make(map[int64]*directStreamHandle),
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

// RegisterDirectStream validates the torrent generation while holding the engine
// read lock, so Delete cannot remove it between validation and registration.
// The caller retains ownership of closer if registration fails.
func (sm *StreamManager) RegisterDirectStream(hash string, expected *ManagedTorrent, closer io.Closer) (<-chan struct{}, func(), error) {
	sm.engine.admissionMu.Lock()
	defer sm.engine.admissionMu.Unlock()
	sm.engine.mu.RLock()
	defer sm.engine.mu.RUnlock()
	if current, ok := sm.engine.managedTorrents[hash]; !ok || current != expected {
		return nil, nil, TorrentNotFoundError{Hash: hash}
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.closed {
		return nil, nil, fmt.Errorf("stream manager closed")
	}
	expected.mu.Lock()
	checking := expected.rechecking || expected.initializing || expected.verificationUncertain
	expected.mu.Unlock()
	if checking {
		return nil, nil, ErrOperationConflict
	}

	sm.nextDirectID++
	id := sm.nextDirectID
	ch := make(chan struct{})
	handle := &directStreamHandle{
		id:         id,
		torrent:    expected,
		closer:     closer,
		cancelChan: ch,
	}
	sm.directStreams[id] = handle

	var once sync.Once
	unregister := func() {
		once.Do(func() {
			sm.mu.Lock()
			delete(sm.directStreams, id)
			sm.mu.Unlock()
		})
	}
	return ch, unregister, nil
}

func (sm *StreamManager) hasReaders(hash string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for key, state := range sm.states {
		if key.Hash == hash && (state.ActiveStreams > 0 || state.ActivePreloads > 0 || state.DebounceTimer != nil) {
			return true
		}
	}
	for _, handle := range sm.directStreams {
		if handle.torrent.hash == hash {
			return true
		}
	}
	return false
}

func (sm *StreamManager) CloseStreamsForTorrent(mt *ManagedTorrent) {
	if !sm.engine.beginWork() {
		return
	}
	defer sm.engine.wg.Done()
	sm.engine.admissionMu.Lock()
	handles := sm.detachStreamsForTorrentLocked(mt)
	sm.engine.admissionMu.Unlock()
	closeDirectHandles(handles)
}

func (sm *StreamManager) detachStreamsForTorrentLocked(mt *ManagedTorrent) []*directStreamHandle {
	sm.mu.Lock()
	for key, state := range sm.states {
		if state.Torrent == mt {
			if state.DebounceTimer != nil {
				state.DebounceTimer.Stop()
			}
			for _, session := range state.Sessions {
				close(session.Done)
			}
			delete(sm.states, key)
		}
	}
	var toClose []*directStreamHandle
	for id, handle := range sm.directStreams {
		if handle.torrent == mt {
			toClose = append(toClose, handle)
			delete(sm.directStreams, id)
		}
	}
	sm.mu.Unlock()
	return toClose
}

func closeDirectHandles(toClose []*directStreamHandle) {
	for _, handle := range toClose {
		close(handle.cancelChan)
		if handle.closer != nil {
			_ = handle.closer.Close()
		}
	}
}

func (sm *StreamManager) CloseAllStreams() {
	sm.engine.admissionMu.Lock()
	sm.mu.Lock()
	sm.closed = true
	for key, state := range sm.states {
		if state.DebounceTimer != nil {
			state.DebounceTimer.Stop()
		}
		for _, session := range state.Sessions {
			close(session.Done)
		}
		delete(sm.states, key)
	}
	toClose := make([]*directStreamHandle, 0, len(sm.directStreams))
	for id, handle := range sm.directStreams {
		toClose = append(toClose, handle)
		delete(sm.directStreams, id)
	}
	sm.mu.Unlock()
	sm.engine.admissionMu.Unlock()
	closeDirectHandles(toClose)
}

// AddStream increments the reference count for a file and applies a range-aware QoS overlay.
func (sm *StreamManager) AddStream(ctx context.Context, hash string, fileIndex int, opts StreamOptions) error {
	if opts.IsHEAD || opts.SkipQoS {
		logging.Debugf("stream QoS skipped hash=%s file_index=%d head=%t skip=%t", hash, fileIndex, opts.IsHEAD, opts.SkipQoS)
		return nil
	}
	if ready, _, err := sm.engine.CheckFileDirectReady(hash, fileIndex); err == nil && !ready {
		if err := sm.engine.CheckPayloadAdmission(hash); err != nil {
			return err
		}
	}

	sessionID, err := sm.addSession(hash, fileIndex, opts, false)
	if err != nil {
		return err
	}

	// Watch for context cancellation to remove the stream.
	sm.watchSession(ctx, hash, fileIndex, sessionID, true)

	return nil
}

func (sm *StreamManager) AddPreload(ctx context.Context, hash string, fileIndex int, opts StreamOptions) (func(), error) {
	if opts.SkipQoS || opts.IsHEAD {
		return func() {}, nil
	}
	if ready, _, err := sm.engine.CheckFileDirectReady(hash, fileIndex); err == nil && !ready {
		if err := sm.engine.CheckPayloadAdmission(hash); err != nil {
			return nil, err
		}
	}

	sessionID, err := sm.addSession(hash, fileIndex, opts, true)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			sm.removeSession(hash, fileIndex, sessionID, false)
		})
	}
	sm.watchSession(ctx, hash, fileIndex, sessionID, false)
	return cleanup, nil
}

func (sm *StreamManager) watchSession(ctx context.Context, hash string, index int, id int64, debounce bool) {
	sm.mu.Lock()
	state := sm.states[FileKey{Hash: hash, Index: index}]
	if state == nil {
		sm.mu.Unlock()
		return
	}
	session, ok := state.Sessions[id]
	owner := state.Torrent
	sm.mu.Unlock()
	if !ok || owner == nil {
		return
	}
	sm.engine.RunStorageTask(func(engineCtx context.Context) {
		select {
		case <-session.Done:
			return
		case <-ctx.Done():
		case <-owner.ctx.Done():
		case <-engineCtx.Done():
		}
		sm.removeSession(hash, index, id, debounce)
	})
}

func (sm *StreamManager) addSession(hash string, fileIndex int, opts StreamOptions, preload bool) (int64, error) {
	sm.engine.admissionMu.Lock()
	defer sm.engine.admissionMu.Unlock()
	if sm.engine.ctx.Err() != nil {
		return 0, sm.engine.ctx.Err()
	}
	mt, file, err := sm.streamFile(hash, fileIndex)
	if err != nil {
		return 0, err
	}
	if !opts.AccountingOnly && !file.ReadyForDirectRead() {
		if err := sm.engine.admitPayloadLocked(mt); err != nil {
			return 0, err
		}
	}
	sm.mu.Lock()
	if sm.closed {
		sm.mu.Unlock()
		return 0, context.Canceled
	}

	key := FileKey{Hash: hash, Index: fileIndex}
	state, exists := sm.states[key]
	logging.Infof("stream add requested hash=%s file_index=%d existing=%t", hash, fileIndex, exists)

	if !exists {
		state = &StreamState{
			Torrent:  mt,
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
	state.TimerVersion++

	sm.nextSessionID++
	sessionID := sm.nextSessionID
	state.Sessions[sessionID] = StreamSession{
		Done:           make(chan struct{}),
		ID:             sessionID,
		Options:        opts,
		Preload:        preload,
		AccountingOnly: opts.AccountingOnly,
	}
	if preload {
		state.ActivePreloads++
		logging.Debugf("stream preload reference incremented hash=%s file_index=%d active_preloads=%d", hash, fileIndex, state.ActivePreloads)
	} else {
		state.ActiveStreams++
		logging.Debugf("stream reference incremented hash=%s file_index=%d active=%d range_start=%d has_range=%t", hash, fileIndex, state.ActiveStreams, opts.RangeStart, opts.HasRange)
	}
	sm.mu.Unlock()

	if err := sm.refreshTorrentOverlayLocked(mt); err != nil {
		sm.rollbackSessionLocked(hash, fileIndex, sessionID)
		return 0, err
	}
	return sessionID, nil
}

func (sm *StreamManager) rollbackSession(hash string, fileIndex int, sessionID int64) {
	sm.engine.admissionMu.Lock()
	defer sm.engine.admissionMu.Unlock()
	sm.rollbackSessionLocked(hash, fileIndex, sessionID)
}

func (sm *StreamManager) rollbackSessionLocked(hash string, fileIndex int, sessionID int64) {
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
	close(session.Done)
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
	sm.engine.admissionMu.Lock()
	defer sm.engine.admissionMu.Unlock()
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
			sm.scheduleDebounceLocked(hash, fileIndex)
		}
		return
	}
	sm.mu.Unlock()

	if sessionID != 0 {
		sm.removeSessionLocked(hash, fileIndex, sessionID, true)
	}
}

func (sm *StreamManager) removeSession(hash string, fileIndex int, sessionID int64, debounce bool) {
	sm.engine.admissionMu.Lock()
	defer sm.engine.admissionMu.Unlock()
	sm.removeSessionLocked(hash, fileIndex, sessionID, debounce)
}

func (sm *StreamManager) removeSessionLocked(hash string, fileIndex int, sessionID int64, debounce bool) {
	sm.mu.Lock()

	key := FileKey{Hash: hash, Index: fileIndex}
	state, exists := sm.states[key]
	if !exists || state == nil {
		sm.mu.Unlock()
		return
	}

	session, sessionExists := state.Sessions[sessionID]
	if !sessionExists {
		sm.mu.Unlock()
		return
	}
	if sessionExists {
		delete(state.Sessions, sessionID)
		close(session.Done)
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
		if err := sm.refreshTorrentOverlayLocked(state.Torrent); err != nil {
			logging.Warnf("failed to refresh stream QoS hash=%s file_index=%d: %v", hash, fileIndex, err)
		}
		return
	}

	if debounce {
		sm.scheduleDebounceLocked(hash, fileIndex)
		return
	}
	sm.clearStreamOverlayLocked(hash, fileIndex)
}

func (sm *StreamManager) scheduleDebounce(hash string, fileIndex int) {
	sm.engine.admissionMu.Lock()
	defer sm.engine.admissionMu.Unlock()
	sm.scheduleDebounceLocked(hash, fileIndex)
}

func (sm *StreamManager) scheduleDebounceLocked(hash string, fileIndex int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	key := FileKey{Hash: hash, Index: fileIndex}
	state, exists := sm.states[key]
	if !exists || state == nil {
		return
	}
	if sm.closed || state.ActiveStreams != 0 || state.ActivePreloads != 0 {
		return
	}
	if state.DebounceTimer != nil {
		state.DebounceTimer.Stop()
	}

	state.TimerVersion++
	version := state.TimerVersion
	state.DebounceTimer = time.AfterFunc(DebounceDelay, func() {
		sm.engine.RunStorageTask(func(context.Context) { sm.expireDebounce(key, state, version) })
	})
	logging.Debugf("stream debounce scheduled hash=%s file_index=%d delay=%s", hash, fileIndex, DebounceDelay)
}

func (sm *StreamManager) expireDebounce(key FileKey, expected *StreamState, version uint64) {
	sm.engine.admissionMu.Lock()
	defer sm.engine.admissionMu.Unlock()
	sm.mu.Lock()
	state := sm.states[key]
	valid := !sm.closed && state != nil && state == expected && state.TimerVersion == version && state.ActiveStreams == 0 && state.ActivePreloads == 0
	sm.mu.Unlock()
	if valid {
		sm.clearStreamOverlayLocked(key.Hash, key.Index)
	}
}

func (sm *StreamManager) refreshStreamOverlay(hash string, fileIndex int) error {
	sm.engine.admissionMu.Lock()
	defer sm.engine.admissionMu.Unlock()
	mt, _, err := sm.streamFile(hash, fileIndex)
	if err != nil {
		return err
	}
	return sm.refreshTorrentOverlayLocked(mt)
}

// admissionMu protects the entire snapshot/apply/publish sequence, including
// shared boundary pieces belonging to sessions on different files.
func (sm *StreamManager) refreshTorrentOverlayLocked(mt *ManagedTorrent) error {
	if !sm.engine.currentTorrent(mt) {
		return context.Canceled
	}
	if mt.t.Info() == nil {
		return nil
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.closed {
		return context.Canceled
	}
	mt.mu.Lock()
	inactive := !mt.policy.payloadEligible()
	mt.mu.Unlock()
	var oldRanges, newRanges []streamPieceRange
	files := mt.t.Files()
	for key, state := range sm.states {
		if key.Hash != mt.hash || state.Torrent != mt {
			continue
		}
		oldRanges = append(oldRanges, state.AppliedRanges...)
		var ranges []streamPieceRange
		if !inactive && key.Index >= 0 && key.Index < len(files) {
			file := files[key.Index]
			state.FullyDownloaded = file.BytesCompleted() == file.Length()
			if !state.FullyDownloaded {
				if len(state.Sessions) == 0 && state.DebounceTimer != nil {
					ranges = cloneStreamPieceRanges(state.AppliedRanges)
				}
				for _, session := range state.Sessions {
					if !session.AccountingOnly {
						ranges = append(ranges, sm.computeStreamPieceRanges(file, session.Options)...)
					}
				}
			}
		}
		state.AppliedRanges = mergeStreamPieceRanges(ranges)
		newRanges = append(newRanges, state.AppliedRanges...)
	}
	oldRanges = mergeStreamPieceRanges(oldRanges)
	newRanges = mergeStreamPieceRanges(newRanges)
	for _, r := range oldRanges {
		for p := r.Begin; p < r.End; p++ {
			if !streamRangesContainPiece(newRanges, p) {
				mt.t.Piece(p).SetPriority(torrent.PiecePriorityNone)
			}
		}
	}
	for _, r := range newRanges {
		for p := r.Begin; p < r.End; p++ {
			mt.t.Piece(p).SetPriority(torrent.PiecePriorityHigh)
		}
	}
	return nil
}

func (sm *StreamManager) clearStreamOverlay(hash string, fileIndex int) {
	sm.engine.admissionMu.Lock()
	defer sm.engine.admissionMu.Unlock()
	sm.clearStreamOverlayLocked(hash, fileIndex)
}

func (sm *StreamManager) clearStreamOverlayLocked(hash string, fileIndex int) {
	key := FileKey{Hash: hash, Index: fileIndex}
	sm.mu.Lock()
	state, ok := sm.states[key]
	if !ok || state == nil || state.ActiveStreams != 0 || state.ActivePreloads != 0 {
		sm.mu.Unlock()
		return
	}
	oldRanges := cloneStreamPieceRanges(state.AppliedRanges)
	if state.DebounceTimer != nil {
		state.DebounceTimer.Stop()
	}
	delete(sm.states, key)
	sm.mu.Unlock()

	// If no ranges were applied (e.g. accounting-only sessions or completed files),
	// do not touch piece priorities and do not restore base priorities.
	if len(oldRanges) == 0 {
		return
	}

	mt, _, err := sm.streamFile(hash, fileIndex)
	if err != nil {
		logging.Warnf("failed to clear stream QoS hash=%s file_index=%d: %v", hash, fileIndex, err)
		return
	}
	if state.Torrent != mt {
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
	inactive := !mt.policy.payloadEligible()
	mt.mu.Unlock()

	if inactive {
		logging.Debugf("torrent is inactive, restoring inactive priorities hash=%s", mt.t.InfoHash().HexString())
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
