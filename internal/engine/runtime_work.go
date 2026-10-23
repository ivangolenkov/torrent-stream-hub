package engine

// runRuntimeWorkLocked is entered and returns with admissionMu held, with no
// suffix lock held. It owns task accounting while doing I/O outside global
// admission, then fences results by runtime pointer, policy epoch and binding.
// The caller applies no result if validation fails; work must not mutate engine
// state, library gates, intent or workflow authority itself.
func (e *Engine) runRuntimeWorkLocked(mt *ManagedTorrent, work func()) error {
	if !e.currentTorrent(mt) {
		return ErrOperationConflict
	}
	if !e.beginWork() {
		return e.ctx.Err()
	}
	defer e.wg.Done()
	mt.mu.Lock()
	epoch, fence := mt.policyEpoch, mt.runtimeFence
	mt.mu.Unlock()
	e.admissionMu.Unlock()
	work()
	e.admissionMu.Lock()
	mt.mu.Lock()
	valid := mt.policyEpoch == epoch && mt.runtimeFence == fence
	mt.mu.Unlock()
	if !valid || !e.currentTorrent(mt) {
		return ErrOperationConflict
	}
	return nil
}
