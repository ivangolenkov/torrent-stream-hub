# Engine concurrency contract

## Current completion publication policy (phase 45)

This section supersedes the earlier phase-40/43/44 **publication snapshot** and
mandatory per-piece-sync claims below, not checkpoint/namespace durability or
range/lifecycle exclusion. MarkComplete still owns a lifecycle read lease. It
takes the existing payload read lease for Set(true), releases it, then attempts
synchronous completion Flush for persistent backends. Set errors return without
Flush/publication. Flush errors are WARN-only with the same timing/error counters.
Nonpersistent completion skips this Flush. No payload lease is held across best-effort
Flush; same-piece or neighbor writes may proceed. No async flush/group commit or
new worker is introduced. Strict and managed persistent backends share this policy.

For VerifyOnOpen, publication then takes its full-piece payload read lease,
acquires CURRENT file evidence and interns/marks it fresh under freshMu. Acquisition
and rooted errors remain errors. There is no comparison with hashSnapshot before
or after Set/Flush, nor a rejection based on the captured signature's validity.
BeginVerification/hashSnapshot remain for checkpoint in-flight exclusion; writes,
revoke and MarkNotComplete still erase them, and successful markFresh consumes them.
They are no longer callback authorization tokens. All remaining references were
audited in storage/file-piece.go, file-torrent.go, file-torrent-io.go,
resume-mutation.go, managed-files.go and checkpoint.go.

Engine pendingWrites still joins network writes before hashing, and byte-range
leases stabilize actual reads and publication steps. Those facts do NOT prove the
callback still describes current bytes between steps. A late callback can override
an erased/revoked snapshot and enter fresh readiness, then a checkpoint. Checkpoint
sync does not rehash that data. This weaker integrity policy is explicitly accepted
in phase 45; prior stale-callback exclusion claims no longer apply.

Checkpoint lifecycle admission, exclusion of active hashSnapshot entries, mandatory
payload/directory sync, strict restore evidence and durable ownership/revocation
are unchanged. Namespace promotion and explicit Flush retain their error behavior.
Flush-error-only publication is permitted, including ENOSPC from Flush; separate
disk-admission/WriteAt errors and known-volume-error cache fencing remain. A generic
non-VerifyOnOpen persistent backend cannot claim that its hints are untrusted on
reopen: returning to legacy Set-before-Flush weakens its crash semantics too.

## Phase 43 checkpoint foundations (blocks 1–2)

Engine.New acquires the rooted persistent `.tsh-instance.lock` before creating
the torrent client/storage. Close releases it only after task joins and backend
close; failed client initialization releases it too. A second cooperating instance
cannot acquire the same file through a root alias. Never unlink the lock file.
This is local-filesystem instance ownership, not exclusion of external writers.

Resume repository calls are not library callbacks. They acquire one of two global
bounded payload working sets BEFORE a SQL connection/transaction. WAL read snapshots
copy bounded serialized state, commit, then decode. Safety writes verify FULL/WAL
on their own connection and use BEGIN IMMEDIATE plus record/mapping/generation/epoch
CAS. Revocation and its integrity digest commit atomically; late seal cannot clear
a newer epoch. No filesystem operations occur inside those SQL transactions.

Normal production sessions now default to fast mode; HUB_FAST_RESUME_MODE=verify
retains strict checking on every open. Mode changes require restart. The use case
installs the periodic/shutdown producer and mutation hooks. The explicit Go mode
constructor also enables guarded writable restore with
those hooks and the validated attachment-scoped protocol. VerifyOnOpen is never
disabled. Backend-open Restore runs inside the library's tracked open generation,
before publication; Drop/Close cancels/joins it and a stale result cannot attach.

Resume diagnosis belongs to the backend open, not a mutable hash-keyed registry.
The factory/restore callback fills scalar outcome facts before publication and
never mutates them afterwards. Current byte sources are tagged in existing native
piece evidence under freshMu; the existing library/client snapshot read path
samples these cached facts without SQL/FS I/O or an additional piece bitmap.
Successful fresh hashing replaces the restored source; counters do not overlap.

Checkpoint IOEvent boundaries (snapshot, per-payload sync and per-directory sync,
before/after) are optional observation only. They run inside the already-owned
seal operation, perform no additional stat/hash/SQL, and grant no authority.
Observers must not reenter storage/library or acquire command keys. An after
event means the platform helper returned successfully; only OS tracing establishes
actual syscall counts for that platform. Crash tests deliberately stop observers
to kill the subprocess at these protocol boundaries, never release a live fence.

`Torrent.FileCheckpointState` samples atomic cached backend revisions under the
library read lock, without stat/hash/flush/SQL. A successful seal acknowledges
only its captured revision; evidence withdrawal during publish stays dirty.
Successful restore acknowledges its own imported revision, not a new download.
Read-side evidence withdrawals advance the revision under freshMu. Restore
rechecks its token epoch under that same lock immediately before atomic import,
after rooted I/O and owner callbacks. Runtime context cancellation is not file
corruption: cancelled reads/probes preserve prior evidence and zero-file proof.

## Phase 43 mutation barriers (block 4)

The main rooted file backend obtains FileResumeHooks once per open, outside
Client.lock. Hooks bind exact record/storage/mapping identity; legacy completion
hints grant nothing. Default verify opens invalidate prior checkpoints before
verification. This untrust operation accepts active recovery journals, unlike
checkpoint admission: an explicit verified recovery must not bypass its journal,
but must still be able to open storage for ordinary hashing.

Backend lifecycle remains the mutation/seal fence. resume.mu only coalesces the
persisted bitmap and its durable callback; it is not another application command
registry. New/untrusted chunks allocate no revoke mask and do no safety SQL.
After a successful seal/restore, overwriting or rejecting a persisted piece must
commit revoke before the effect. An uncertain publish retains the union of old
and possibly committed masks. A discarded generation clears the pending cache.

In strict storage, file payload leases cover actual syscalls: writes take exclusive leases for the
contributing files in offset order, and reads/hash reads take shared leases.
Revoke closes freshness before a write; normal ReadAt checks freshness AFTER its
lease is acquired, closing the available-to-read/overwrite race. Hash WriteTo is
the verification path and may read unverified data. Missing/truncated/replaced
files revoke their whole contributing piece set before create/extension.

Reads do NOT take the exclusive seal admission gate, so publish does not stop
existing read-only streams. Close atomically closes read admission, joins file
leases (including evidence refresh + durable SQL callback), then clears freshness
and closes backend handles. A closed attachment cannot read a reused root.
Namespace commands still use the existing application reservations and Drop/
transfer joins; file leases do not replace ownership or protect external writers.

Known v2 hash replacement revokes outside Client.lock while counted as layer
work, then revalidates backend pointer/hash/closed state before applying it. Drop
joins through storageLock, and Client.Close joins the counted task. A failed revoke
does not apply the new hash and cannot leave known-invalid live readiness.

Empty-file readiness in the rooted verified backend requires cached rooted-open
proof, refreshed outside Client.lock on real readiness checks, never on polling.
Restore candidates do not manufacture missing empty files. Checkpoint flush alone
uses its independent job context, not the cancelled runtime I/O context; ordinary
writers remain cancelled during shutdown. Issued syscalls retain their leases.

## Phase 43 seal worker lifetime (partial block 3)

`RegisterStorageFinalizer` transfers exactly one checkpoint worker's shutdown
ownership, serialized with BeginShutdown by storageTaskMu. Rejection transfers
nothing; the caller stops/joins its worker. No registration is accepted after
runtime cancellation. This slot does not authorize storage access or SQL seal.

Close cancels ordinary admission/work and quiesces managed state/streams, then
requests the registered finalizer's Shutdown with an independent 10-second drain
context outside engine locks, concurrently with engine task joins. Requesting stop
before joins is essential: coordinator cleanup can retain a reference until an
active seal responds to cancellation. After joining ordinary tasks and the
Shutdown call, Close always calls Wait, even after drain failure/deadline. Only
then does it close the torrent client/backend and release the instance lock.
An uninterruptible callback therefore retains ownership rather than being treated
as joined on timeout. Do not create a service task waiting for finalizer.Done:
service tasks may depend on a stop request, but must not depend on finalizer.Wait.

SealWorker uses one goroutine, up to 64 coalesced owners including the active one,
and a 60-second periodic tick. Saturation leaves producer dirty state pending; it
is never a durable revocation queue. Neither worker registration nor its fresh
shutdown context bypasses the cancelled application coordinator. Production
adapter/drain ownership still needs integration before checkpoints are generated
or trusted by normal sessions; no worker is installed by default yet.

## Phase 42 verification and storage open

### Current managed payload model (phase 44 ownership revision)

The application opts into ManagedPayload on writable rooted VerifyOnOpen storage
without part files. Default fork backends and offline analysis remain strict.
External modification of an active attachment is unsupported; no immediate
path/mtime/volume-change detection is promised. Full Recheck is required after
external edits. Cached readiness is attachment-local historical verification,
not current namespace evidence. Rooted security checks remain at acquisition.

The attachment-local LRU holds at most 16 payload descriptors, including reserved
opening/closing slots. Cache metadata is protected by its own short mutex; no
open/close/guard/stat/sync/SQL runs under it. Acquisition occurs under an existing
payload lease or lifecycle lease (completion sync). Same-file openings coalesce;
read-to-write upgrade waits for outstanding leases, closes the old descriptor,
and revalidates the rooted/volume boundary. No unbounded overflow handles or
per-file goroutines are created; a full borrowed pool waits on a cancelable state
notification. Cache errors propagate; a closed parent client rejects cache hits.

Managed range follow-up: `lockPayloadRange` retains the existing per-file
payloadMu only as a SHARED Close/Reset join lease for both reads and writes.
Writes do not exclusively lock the entire file. The same entry point admits
half-open TORRENT BYTE ranges through managedRanges: read/read and disjoint
requests proceed concurrently; intersecting requests with a writer wait. Disjoint
chunks of the same piece are independent too. Hash WriteTo, BeginVerification
and commitCompletion use their full piece extent, including v1 shared boundaries.
Engine pendingWrites already joins network writes before hashing; range exclusion
additionally protects backend ReadAt/WriteTo and publication from direct storage
calls and late overlapping mutations. It is not another trust bitmap or scheduler.

Order is lifecycle read lease for mutations/publication -> shared contributing-file
leases in torrent offset order -> byte-range admission -> existing resume/fresh/cache
metadata locks. No range/cache metadata mutex is held through I/O, sync or SQL.
Earlier conflicting waiters have priority, while unrelated ranges may bypass them.
Each request has one short-lived node, O(active+waiting calls) memory and admission
scan cost, not O(torrent pieces) resident locks. Nodes are removed on all release
paths; no goroutine per range or payload queue/buffer is introduced. Like the prior
native payloadMu waits, range exclusion is synchronous; it does not forcibly cancel
an admitted syscall. Descriptor-capacity waits keep their existing cancellation.
Write wait counters measure contention on these ranges/file join barriers; hold
active may exceed one per file. Cache waits/lifecycle admission are separate.

Writes still revoke the entire affected piece's live/durable trust before the
first byte is changed. Multiple independent chunks can erase the same snapshot;
none can create trust by writing. Under phase 45 a late completion callback CAN
re-grant that trust. Publication's full-piece read lease stabilizes current-evidence
capture and markFresh, but no longer validates the old hash snapshot. Completion
sync now follows Set and is best-effort, outside that lease. No per-piece fsync
removal/group commit is included. File creation/upgrade still coalesces at descriptor acquisition, with
guard/revocation before create; active namespace edits remain unsupported.
Checkpoint takes lifecycle exclusively, joins all mutation calls and holds it
through sync/publish; existing readers can continue. Close/Reset additionally take
all file leases exclusively to join even readers before closing cached handles.
There is no reverse range->file lock acquisition or cache mutex held while waiting
for a range. Strict mode continues exclusive file locking for its mtime accounting.

A known application guard failure at acquisition or checkpoint latches a cache
error, wakes capacity waiters and clears fresh/hashSnapshot/signature validity.
Future hits fail, even for a previously open descriptor. Already admitted I/O is
joined by the existing lifecycle barriers; it cannot be forcibly canceled inside
an OS syscall. The attachment must detach/reopen for recovery. This distinguishes
unsupported immediate external detection from ignoring an observed unsafe volume.

Reads/hashing/completion sync use the same retained inode. Chunk writes do not
capture before/after stat and do not refresh file-wide mtime evidence. Internal
overlapping writes still erase hashSnapshot and revoke live/persisted trust before
mutation; independent pieces retain their shared valid signature. Managed
publication now interns current retained identity without validating the original
hash attempt, as specified in phase 45 above. No size/name/cache-hit alone creates
completion authority without a caller's MarkComplete notification.
ReadAt in managed mode requires cached verified completion; hash WriteTo is the
existing explicit verification path and may read untrusted data. Hash short reads
and non-cancellation I/O failures revoke file trust and retire the affected handle.
Synchronous best-effort completion sync remains, including write-access upgrades
for Windows FlushFileBuffers. Checkpoint sync remains mandatory.

Close/reset acquire lifecycle exclusively, then all payload writer leases,
joining read leases too, before closing retained descriptors. Full VerifyDataContext
captures ResetManagedPayload under Client.rLock then calls it OUTSIDE Client.lock.
Reset closes the live gate first, then attempts durable revocation and cache
release; failure prevents Recheck from proceeding. A callback from an old hash
cannot restore its erased/revoked snapshot. Pause does not detach storage: handles
may remain for seeding/streams. Application namespace mutations detach/Drop first;
part-file promotion is not supported by managed mode. Public piece-level VerifyData
does not reset the entire cache; use full Recheck for external changes.

Checkpoint still excludes active hashes, takes rooted current evidence under the
lifecycle barrier, syncs and checks evidence again before publish. Managed mode
compares retained file identity at this boundary, not stale acquisition mtime
against expected internal writes. PrepareRestore/RestoreCheckpoint retain strict
size/mtime/identity/volume checks. External same-inode changes while attached can
invalidate historical trust without being noticed; this is explicitly outside
the ownership model, not a claim of filesystem scrubbing.

BT Health adds managed_payload and cached hit/miss/eviction/open counters. Open
sub-stages measure real acquisition misses; WriteOpen/WriteClose may merely borrow/
return a lease. The 16-descriptor cap is per attachment, not global. Fixed timing
state is 78 atomic int64 fields (624 B excluding padding), plus bounded cache
metadata/retained file objects; no payload buffers are held in the cache. Hashing
uses a 32 KiB copy buffer per existing bounded worker. Earlier strict-mode lock
and signature notes below apply where not superseded by this managed model.

### Phase 44 selective startup and signature publication

Rooted VerifyOnOpen backends collect ordered, merged `InitialIncomplete` piece
ranges after namespace initialization and validated resume import, before backend
publication. Metadata probes are O(files); interval storage is O(files), without
a new piece bitmap or payload buffers. The client consumes ranges in O(files +
pieces) during initial completion admission and releases them before GotInfo.
Only known incomplete is installed, never trust or a hash count. The resulting
piece flag preserves known incomplete when the first v2 piece layer arrives
(which continues initial admission without changing payload). It does not exempt
subsequent downloads or explicit rechecks, and is cleared when any hash starts.
New complete network pieces always queue hashing. Interval storage is lazily
allocated and capped at 65536 entries (1 MiB on 64-bit); overflow discards the
negative plan and safely falls back to ordinary initial verification.
Unknown access/guard errors take the ordinary error path, not the absent path.

In strict storage, signature capture (`BeginVerification`) and validation/commit (`MarkComplete`)
take existing contributing-file payload read leases in torrent offset order,
inside the lifecycle read lease, before name locks/stat and short freshMu
sections. Internal writers hold the matching payload write lease while changing
bytes and updating shared signatures. No writer can make captured stat older than
the accounted signature within either publication step. The lease is NOT held
across the entire hash attempt: independent piece writes between steps remain
valid; same-piece writes erase hashSnapshot and reject the stale result.

For VerifyOnOpen storage, completion flush now runs BEFORE taking the publication
payload lease, still inside the lifecycle lease and outside freshMu/Client.lock.
The initial phase-44 fix held the file-wide payload lease through fsync and could
stall all neighboring inline network writers on slow storage. Sync itself does
not change payload. After it returns, acquire contributing-file read leases,
capture current stat, validate the original hash snapshot, and publish under the
existing short freshMu sections. A same-piece write during/after sync erases the
snapshot; an independent write updates only shared accounted file evidence. A
replace/truncate is rejected by signature comparison. No newer same-piece write
can slip between the post-sync validation and publication. Durability of the
unchanged verified piece precedes publication; unrelated later writes gain no
trust. Legacy non-VerifyOnOpen backends retain the old sync lease scope because
they have no hash-snapshot fence. Sync is not disabled, batched or made async.

Hint persistence also runs outside freshMu and Client.lock. The snapshot is
revalidated under freshMu after hint persistence, so an intervening revoke/layer
invalidation cannot be resurrected. MarkNotComplete also removes the snapshot.
Release the publication payload lease before promotion/refresh: recursive RLock
can deadlock behind a queued writer. Close/checkpoint retain their existing
lifecycle exclusive fences. A failed MarkComplete never installs library complete
or announces HAVE; it keeps requests eligible, without busy retry or peer blame.
Filesystem errors stay visible rather than being downgraded to successful checks.

BT Health `torrents[].storage_io` copies attachment-local atomic counters only:
completion-sync attempts/errors/active/total ns/max ns and contended payload-write
lease acquisitions/active/total ns/max ns. Completion sync measures the entire
piece Flush call (including opens, name-lock waits and all contributing files),
not isolated kernel fsync duration. Write-wait counters are per contended file
lease, not per peer or piece; they exclude lifecycle/seal, SQL, device-internal
waiting and the write syscall itself. Totals can exceed wall time under
concurrency; snapshots are approximate across fields. No filesystem calls,
payload/lifecycle lock acquisition, per-piece logs or unbounded samples are added
by polling. Nine base int64 atomics plus eight fixed stage records of five int64
atomics each (392 bytes excluding padding) per backend open; no new workers/queues.

Write stages additionally expose prepare (piece revoke/live invalidation), guard
(persisted-file revoke), open (including application IOGuard/volume validation),
descriptor stat, payload WriteAt, shared-signature accounting, descriptor close,
and fully acquired range lease hold. Hold starts after the entire range is locked;
partial acquisition waits remain in the per-file wait counters. Hold errors are
always zero; other errors describe stage results, not final WriteAt return status.
Stat samples normally number two per contributing file; root open's own stat
checks are part of open. A short payload write counts as an error even when the
backend returned nil error. Existing delayed-close/short-write error precedence
and stat-error return behavior are unchanged. Stage totals nest/overlap and cannot
be summed to infer disk utilization. Polling still only loads cached atomics.

For rooted descriptor-only writers, VerifyOnOpen and disabled part files, the
final contributing descriptor Close runs after releasing the publication/mutation
payload lease. All writes and before/after shared-signature accounting complete
before release. No later extent in that call can mutate a newly observed range.
The lifecycle lease is held through Close, so seal/Close/drop still join it, and
errors still reach the writer. A private capability opts in only descriptor-only
close; mmap/buffered/unknown writers retain exclusion. Part-file promotion and
legacy verification also retain the old scope to avoid widening namespace and
legacy publication races. Intermediate extent Close remains under the original
whole-range lease. This is a bounded narrowing, not concurrent same-file writes:
open/stat/write/accounting still serialize to preserve shared signature evidence.

Root-open follow-up adds five write-only sub-stages: application guard, rooted
path check/pre-open identity, mkdir, descriptor open and descriptor identity
validation. They are children of write_open; read/sync opens do not increment
them. Expected ENOENT for create is not an error. Optional instrumentation is nil
for public OpenRootFile/non-attached callers. Five additional fixed records bring
the total to 74 atomic int64 fields (592 bytes excluding padding) per attachment.

Rooted open now performs lexical conversion before the application guard, then
one complete filesystem path validation after it; it no longer walks the same
path before AND after the callback. Guard input is a lexically safe root-relative
name, not filesystem trust. The guard still executes for every open, without TTL
or cached positive volume evidence. Any callback-installed symlink is rejected by
the subsequent validation; descriptor identity is still compared with the captured
pre-open identity. MkdirAll runs only for missing entries. Existing non-exclusive
opens drop O_CREATE, so a disappearance after validation cannot synthesize an empty
replacement before rejecting identity. O_EXCL behavior is unchanged. Cancellation
is rechecked after a delayed guard and before payload open/create. No write lease,
durable revoke, lifecycle fence or required sync was removed.

FileObservations derives skipped_missing_bytes only for initially skipped pieces
with no subsequent hash attempt and no queued/active work. REST/SSE copies those
cached facts, without filesystem work. Skips neither grant readiness nor advance
the actual hash progress timestamp. The observation is not current inventory.

Main (two slots, one per torrent) and offline (one slot) clients share one
`HashBudget(2, 1, 64, 8)`: two active hash read/compute tasks, one per live
filesystem, 64 pending leases, a FIFO turn after eight priority grants. Existing
slots bound waiters; no worker is spawned per queued piece. Resource keys come
from rooted directory descriptors (`st_dev` / Windows volume serial), outside
Client.lock. A boundary piece acquires all contributing keys atomically. Release
follows the storage read lock, before reacquiring Client.lock; completion flush/SQL
is outside that physical hash lease and retains normal slot limits.

Every piece returns to the scheduler. Reader ranges, readahead and durable selection
precede background work; oldest torrent plus cyclic background cursor prevent
starvation. Candidate selection takes one pass with constant scratch space; bitmaps
coalesce duplicate ranges. Selection does not enable networking. `hashWaiting` is
atomic: Client.lock acquisition under storageLock would deadlock with v2 updates.
Layer updates wait for storage outside Client.lock, then revalidate closed/unchanged
state under the original Client -> storage order. Pending layer writers suppress
new hash admission for that torrent until the batch completes, so a long backlog
cannot repeatedly take their storage lock turn. Other torrents remain schedulable.

OpenTorrent (imports, volume checks, empty files) runs outside Client.lock. Its
opening generation is published first; Close/Drop revoke and join it. The owner
closes a late result instead of attaching it. Drop joins outside Client.lock.
Client.Close also joins storage tasks removed from its map by concurrent Drop
before closing the default backend. Uncancellable syscalls retain ownership.

FileObservations and REST/SSE use cached facts, without stat/hash. Inventory uses
one worker, 64 records per round, 128 files per visit, mapping/session fences.
Real reads recheck freshness/volume admission. One location worker shares a mutation
ticket with legacy recovery, with a durable queue of 64 and bounded manifests/buffers.
Enrollment can take this ticket for an interrupted journal without advancing it.
See `tasks/phase42-storage-contract.md` and the measurement report.

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
