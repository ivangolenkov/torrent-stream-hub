# Engine concurrency contract

## Phase 41 storage worker contract

The normal migration worker remains sequential. Ownership retries wake that worker,
not a second migration executor. Heavy explicit recovery uses one global work ticket
and the existing bounded engine task owner; saturation returns conflict instead of
creating a worker per update/request. Shutdown cancels admission/waits but joins the
resource-owning task before closing storage/SQLite. A stuck syscall is not released
just because its HTTP waiter was cancelled.

Application path reservations protect preflight-to-publication while releasing
record handles for long hashes/copies. Their registry mutex owns admission only;
filesystem and metainfo reads are outside it, and publication SQL is short. The
storage alias callback may consult that registry/SQLite under the library callback,
but never waits for record handles or performs filesystem I/O there.

Offline candidate verification uses a separate network-disabled client, one hasher,
rooted read-only plain-name storage and in-memory completion. It neither creates
zero-length payload nor imports/renames candidates. Read callbacks only increment
in-memory counters outside the library lock. They never query SQLite, log chunks,
or perform more filesystem I/O. The normal two-second latest-only snapshot sampler
stamps heartbeat independently from last-progress; only actual reads/file completion
advance progress. Physical safety re-reads count within their stage, not a fictitious
global percent.

## Ownership and lock order

Application command handles (sorted hash/root keys; see `usecase/STATE.md`) ->
`admissionMu` -> `Engine.mu` -> `StreamManager.mu` -> `ManagedTorrent.mu` -> library.
Shorter suffixes are permitted. Never acquire admissionMu while holding a suffix
lock, or wait for an application handle from an engine/library callback. Registry
and policy-data mutexes are not held while waiting for command handles or I/O.
Repository has no application mutex. SQLite write transactions are short and use
BEGIN IMMEDIATE; read snapshots remain deferred/WAL and do not reserve the writer.

`admissionMu` protects reservations/map membership and state/gate/priority effects,
not whole commands or scheduler passes. ManagedTorrent.mu owns mutable runtime
fields. Hash, raw pointer and lifecycle context are immutable after publication.
Client/storage pointers remain immutable through Close for diagnostic readers;
cancellation/work admission represents shutdown. Snapshot readers may take owner
locks without admissionMu, but never apply effects from an unchecked snapshot.

`ManagedTorrent.policy` is independent applied run/slot/operation/completion/fault
authority. Phase/error are projection caches. All state changes enter the common
event executor; raw phase assignment is not a command or slot capability. Library
status callbacks capture generation through an independent sync.Map and enqueue
owned work; they never acquire application/map/stream/torrent locks synchronously
under the library lock. Persistence callbacks touch only SQLite.

## Reserve, work, validate

Scheduler refresh/stat/probe runs outside global admission. Per-runtime health
tickets coalesce work without making other passes wait for a blocked syscall.
Results validate pointer, policy epoch and runtime fence. Every grant rechecks
the current FIFO head and actual reservation count, including pending preparation;
an earlier sorted list/local count cannot authorize a later grant. Grants issue
an attachment-local slot token. Failed cleanup can release only its own token.

Registration runs the first scheduler pass; the background resource monitor starts
with its five-second tick. It does not launch a second startup pass that can race
registration/initial commands and cause their pass to skip an already-owned health
probe. Periodic passes still coalesce per attachment and never wait on another
pass's filesystem work. Tests changing scheduler configuration use admissionMu,
the same lock as scheduler readers.

`admitPayloadLocked` is entered/returns with admissionMu held, but releases it for
probe and copied metainfo persistence. It owns a pointer preparation ticket with
policy epoch, runtime binding and slot token. Concurrent preparation for one raw
runtime conflicts. No suffix lock may span this call. Validation precedes priority
or Allow effects; Pause/delete/re-add/new attachment cannot be undone by stale I/O.

Resume/FinishRestore disk/cache reads, metadata persistence and read-time absence
checks use `runRuntimeWorkLocked`: owned task accounting, unlock, work, reacquire,
validate generation/epoch/binding, then effects. Unknown/forbidden activation is
rejected before I/O. Explicit Resume alone probes/clears the disk latch, with the
synchronous library write-error generation rechecked before clear/Allow. Failed
SQLite persistence keeps live gates closed and does not promise crash durability.

Registration reserves an unpublished pointer in `registering` while cache I/O is
outside admissionMu. Duplicate/alias registration and Delete conflict with it.
Only a valid prepared generation publishes; failure transitions to a `detaching`
namespace reservation through raw Drop. Explicit Delete likewise terminalizes,
cancels, removes map/session identity and reserves its namespace before unlock.
Direct descriptors are detached under bookkeeping locks, then closed outside
them. Drop runs outside admissionMu; same-namespace re-add remains blocked until
Drop/closers finish. All of this is owned work joined by Close.

Library methods still have their own synchronization; this is not a promise to
bypass a library lock, SQLite's single writer or an uninterruptible OS syscall.
No engine-owned hash/copy/unlink/probe/Drop, JSON/SSE or task join intentionally
retains global admission/application locks.

## Verification, selection and sessions

Explicit/automatic checks share one verifier per generation and a distinct
`verificationAttempt` capability, independent of public runtime-only history.
Completion validates current pointer/attempt before persistence/effects. A stale
generation may finish only its own cancelled history. Failed verification keeps
read/cache/upload/activation vetoes until successful local retry; success removes
only its verification fault, never another safety key, paused intent or disk latch.
Verification releases background reservations, never restores a pre-check phase.

Selected completion is distinct from whole-payload completion. Slot release
removes base priorities, not independent stream ranges. Skip All is not Seeding.
Final payload admission separately checks disk/write error facts. Staging/full
Pause and apply failure quiesce through the common executor; applied zero is not
proof that a new intent committed. Only full successful apply/bind acknowledges it.

StreamManager.mu owns sessions/ranges/readers. Session IDs are manager-wide
monotonic tokens; pointer and timer version fence old debounce callbacks. Reconcile
uses admissionMu and stream lock through bounded library priority application to
protect cross-file pieces. Direct descriptor registration validates generation and
verification gates atomically; it never grants payload download. HEAD creates no
activity. Session/direct-reader counts may overlap for one HTTP request.

Preload's own mutex is never held while acquiring application/admission handles.
Capacity tokens survive replacement and release after task/lease cleanup; runtime
contexts invalidate results. The speed sampler alone owns statistic baselines.
Read stamps validate pointer/map membership, policy epoch, binding, applied revision
and failure identity; live byte/peer/speed/activity remain sampled telemetry.

## Shutdown and publication

Main registers signal cancellation before initialization and binds HTTP before
starting persisted restore. The server owns and joins the startup/periodic worker
goroutine on both cancellation and listener failure, before deferred engine and
database closure. Startup preserves restore -> temporary cleanup -> initial
snapshot order. HTTP gates torrent operations with 503/Retry-After until this
sequence finishes; static assets, echo, logs and bounded SSE subscriptions do not
enter engine operations. SSE initially sends only a comment and receives the first
published snapshot. It never presents an empty list as a completed startup result.
Restore passes cancellation through DB snapshot reads and record-handle waits;
per-torrent storage/filesystem operations retain the engine's lifetime context.

storageTaskMu guards task admission/WaitGroup.Add against cancellation. Application
coordinator admission closes with Engine context, cancels waiters and joins command
handles without engine/registry/record locks. Close cancels first, quiesces, detaches
streams, joins owned tasks/commands, then closes library/storage. Synchronous
registration, unlocked preparation and detach are accounted work. Never join work
under admissionMu: it may need that lock to validate/retire. Direct closer callbacks
must not synchronously join the Delete/Close that is closing them.

The fork clears unread upload requests and buffered responses when a peer closes.
A request reader returning after Torrent.Drop must find its request retired even
when storage returns a closed error; it cannot publish bytes to the closed peer or
leave the request-server progress assertion unsatisfied.

SSE sampler ordering is separate from commands. JSON/network writes occur outside
command/global locks. Subscription publication/close share their own short lock;
mailboxes are latest-only, and data/version are paired atomically. Capture gets an
Engine-scoped random epoch/decimal sequence before encoding; command receipts
retire earlier in-flight snapshots, including empty lists. Timestamp is diagnostic.

SQLite is not atomic with filesystem/library apply. Committed desired intent is
not rolled back by observations; deletion/migration journals survive partial
failure. Context/deadlines limit caller waits, not arbitrary running syscalls.

## Phase 40 budgets and I/O boundaries

Task admission has independent caps: 64 service tasks, 256 metadata watchers,
2 verification jobs, `MaxActiveStreams + 4` stream watchers and 256 QoS file states.
Two event workers retain at most 128 coalesced owner/kind events. Optional duplicate
telemetry can be dropped; storage-error gates close synchronously in the library,
and pending durable effects are retried by maintenance. Command admission failure
is an error, never an unowned overflow goroutine or false success.

All HTTP payload endpoints share the positive MaxActiveStreams limit (default 4).
Reservation precedes record/read preparation. Release is idempotent and separate
from playback debounce; HEAD owns only the short record lease. Transfer/metadata/
read preparation propagates caller cancellation. Main connects HTTP BaseContext to
engine lifetime, interrupts outstanding body reads, cancels engine admission before
joining sync/cleanup, closes SSE, drains HTTP, joins engine work, then closes storage,
completion and SQLite. HTTP's drain deadline may force connection closure; it never
closes storage beneath an already-running filesystem syscall. Such a syscall may
extend process exit beyond the HTTP deadline.

Verified storage uses shared per-file signatures and incremental piece counts.
Internal writes touch affected ranges/file signatures; external inode/size/mtime
changes revoke all dependent pieces. Range refresh stats unique files outside the
client lock, then validates the same storage generation before updating completion.
Read/upload/direct-open/handshake boundaries refresh before consuming cached bits.
A cached bit, final name or SQLite hint alone is never proof of readable bytes.

Completion Get/Set run only bounded memory operations under library callbacks.
The adapter has one writer, 4096 LRU entries, 1024 pending hints plus a 1024-entry
in-flight batch. Lost hints are observable and safe because open always verifies.
Close stops admission and joins the writer before SQLite closes. Application
contexts bound queue/SQLite waits, not an arbitrary blocked filesystem syscall.
