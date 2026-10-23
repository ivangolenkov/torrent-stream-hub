# Engine concurrency contract

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

storageTaskMu guards task admission/WaitGroup.Add against cancellation. Application
coordinator admission closes with Engine context, cancels waiters and joins command
handles without engine/registry/record locks. Close cancels first, quiesces, detaches
streams, joins owned tasks/commands, then closes library/storage. Synchronous
registration, unlocked preparation and detach are accounted work. Never join work
under admissionMu: it may need that lock to validate/retire. Direct closer callbacks
must not synchronously join the Delete/Close that is closing them.

SSE sampler ordering is separate from commands. JSON/network writes occur outside
command/global locks. Subscription publication/close share their own short lock;
mailboxes are latest-only, and data/version are paired atomically. Capture gets an
Engine-scoped random epoch/decimal sequence before encoding; command receipts
retire earlier in-flight snapshots, including empty lists. Timestamp is diagnostic.

SQLite is not atomic with filesystem/library apply. Committed desired intent is
not rolled back by observations; deletion/migration journals survive partial
failure. Context/deadlines limit caller waits, not arbitrary running syscalls.
