# Engine concurrency contract

`admissionMu` serializes runtime registration/removal, state commands, scheduler
slot allocation and all file/piece priority side effects (including stream
reconcile). A command takes effect while holding this lock. No stale snapshot may
be applied after releasing it. Runtime identity is the `*ManagedTorrent`, not hash.

Lock order: repository Lifecycle (application operations only) -> admissionMu ->
Engine.mu (map/client snapshot) -> StreamManager.mu -> ManagedTorrent.mu -> library.
Shorter suffixes are permitted. Never acquire admissionMu while holding a suffix
lock. Release map locks before waiting on I/O. Library callbacks enqueue tasks;
they must not synchronously acquire application locks under the library lock.
Snapshot readers may use map/stream/torrent locks without admissionMu but never
apply side effects. ManagedTorrent.mu owns all mutable torrent fields; hash,
library pointer and lifecycle context are immutable after publication.
Client/storage pointers also remain immutable after publication, including after
Close, for diagnostic readers. Engine context represents their shutdown state.
Library status events capture runtime identity through an independent sync.Map and
enqueue owned tasks; they do not acquire map/stream/torrent locks synchronously.

Stream mutation and reconcile use admissionMu; StreamManager.mu protects readers
of bookkeeping as well. Session IDs are manager-wide monotonic tokens. State
identity and timer version invalidate old debounce callbacks. Reconcile holds
stream lock through library apply and publication, protecting cross-file pieces.
Direct descriptor registration uses Engine.mu -> StreamManager.mu to validate
identity atomically against Delete; it never applies priorities or grants payload
admission. The engine's Delete uses the already-admission-locked close helper.

storageTaskMu guards task admission and WaitGroup.Add against Close cancellation.
Tasks inherit Engine context and optionally runtime context. Close cancels first,
closes streams, joins tasks without application locks, then closes library/storage.
Synchronous add/restore also register as work so Close cannot close their client.
Delete cancels runtime work and invalidates map/session identity before Drop;
callbacks validate identity under admissionMu before side effects. Never join a
task while holding admissionMu: that task may need the lock to observe cancellation.
Drop may wait for the library's storage close while holding admissionMu: that
close does not join engine tasks or acquire application locks. Direct descriptor
close likewise must not call back into engine commands. Timer dispatch and the
reader context AfterFunc only enqueue owned work/cancel a context, respectively;
they cannot apply state or storage side effects after task admission closes.

SQLite and library are not one transaction: priorities commit before runtime
apply; failed activation stays gated. Deletion commits intent before Drop/unlink,
and keeps intent on any later failure. Snapshot writes do not overwrite intent.
Context cancellation cannot interrupt an arbitrary already-running OS syscall.
