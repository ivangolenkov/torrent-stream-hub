# Application state and coordination contract

## Phase 42 locations, inventory and files

`torrent_storage` owns placement: layout 0 shared legacy, 1 isolated hash, 2 readable
root with an explicit metadata prefix. `storage_id` is stable across aliases;
`revision` changes with mapping. Record ID, desired revision, operation version,
inventory session and runtime attachment remain distinct fences. Every consumer
resolves persisted placement. SQL upgrade and title edits do not rename files;
magnet reservation precedes its first payload open.

Move/locate reuse `storage_recovery`. `storage_claims` protect source/staging/target
and retained paths beyond record lifetime. Mapping/alias publication commits
atomically; manifests retain their version. Before-publication cancel differs from
after-publication recovery. Locate retains the source, clears its old expected
inventory and commits pause. Copy digests/offline checks never grant runtime readiness.

`storage_volumes` and `.tsh-volume` record enrollment; live filesystem identity
closes admission on ordinary mount replacement/loss. Old installations require
explicit reconcile, including ones containing only interrupted journals. Reconcile
serializes against storage mutation and leaves blocked workflows intact. It cannot
recreate an absent registered marker. Inventory observations are fenced by session,
mapping, record and catalog. Verified observations can establish expected presence
without per-chunk SQL. A disappeared expected file differs from a never-created
skipped file. Explicit Resume authorizes repair after volume validation only.

File state separates presence, history/provenance, checked/verified bytes, effective
priority and direct readiness. Unknown metadata paths are not guessed from display
names. Selected progress has its own denominator and desired revision; empty
selection is neither 100% nor Seeding. DTO readiness is observational: actual opens
enforce current gates again. DiskFull may permit verified read-only access; skipped
complete files remain readable. Details identity includes mapping and attachment.
See `tasks/phase42-storage-contract.md` and the README rehearsal procedure.

## Phase 41 storage recovery

`storage_catalog_version` orders identity/mapping/cleanup/catalog changes without
including byte telemetry. Ownership reads use one short WAL snapshot; metainfo and
filesystem work happens after commit. Publication validates its version and installs
a path reservation under short application admission, never a global engine lock.
Add/alias publication checks active reservations. Migration and deletion retain them
through the final filesystem effect; disappear/re-add never means blindly ignoring
`ErrNoRows`. Retry is bounded (16 cancelable, backed-off attempts); churn is a
temporary diagnostic, not a metainfo corruption diagnosis.

`storage_recovery` is durable per-record workflow state. Record ID and a distinct
operation version fence commands/callbacks. Analysis and explicit verification advance
that version; journal retry retains its chosen operation. Layout, runtime attachment,
metadata-known and current-open readiness remain separate authorities. REST/SSE use
the same diagnostic projection, with optional observational progress layered only
onto the matching record/version. Heartbeat is sampling activity, not I/O completion.

Deletion intent and its initial recovery row commit together. Stages are intent,
runtime stopped, manifest ready, removing, removing record; cancellation commits
`cancel_requested` before joining the record owner. The next unlink checks that
marker; the final SQL delete also rejects it. `cancelled` is committed only after
the worker is gone. It clears admission markers but leaves paused intent and a
partial/unknown-data diagnosis. A disconnected waiter leaves durable cancellation;
startup completes it before attachment. Temporary and manual deletion share this
lifecycle. Completed cancellation cannot be revived by retry or an old version.

Legacy analysis is explicit, bounded and offline. Journal transitions are analyzed,
prepared (choice durable), preserved (alternative moved/flushed), published, verified.
Identity/fingerprints and metainfo membership are revalidated; journal paths never
authorize arbitrary paths. Native rooted no-replace moves preserve the alternative
in `.tsh-recovery`, which is protected independently of torrent record lifetime.
The selected payload stays paused; offline verifier completion never grants a new
runtime readiness. Backup retention survives successful Resume and ordinary cleanup.
Incomplete migration manifests own their paths and exclude recovery admission.

See README and `tasks/phase41-recovery-contract.md` for commands, codes, upgrade
rehearsal and downgrade restrictions. Old cleanup manifests import `removed_data=unknown`;
old storage journals stay unchanged. Recovery progress never causes per-chunk SQL.

## Authorities

| Concern | Authority / permitted writer |
| --- | --- |
| Run, base priorities, metadata, temporary policy, FIFO | SQLite; explicit commands |
| Catalog | Validated hash-matching metainfo; defaults on first insert only |
| Runtime run/slot/operation/fault/completion | Current Engine generation's pure event policy |
| Last-known bytes/phase | Conditional observation writer; never upsert/intent |
| Stream/preload ranges | Current-generation session managers |
| Disk/verification/metadata/selection/missing blocks | Separate typed safety producers/recovery capabilities |
| Deletion/migration | Durable mode/manifest/journal workflows |
| REST/SSE/TorrServer/Vue | Shared projection, not a command authority |

Enabled means scheduler eligibility, not Downloading/readiness. Streaming is
orthogonal activity; selected completion is not whole-payload Seeding. Historical
Streaming/Downloading/Seeding are compatibility imports, not live facts. Unknown
legacy errors restore conservatively; NoPeers/tracker diagnostics do not imply
invalid payload. Only current-open verification grants byte readiness/upload.

## Identity, commands and failures

Record ID changes on delete/re-add; runtime attachment and observation sequence
fence generations/order. Desired revision advances at SQLite commit; applied
revision is a separate live acknowledgment. Versions are decimal strings in JSON.
Old/missing records cannot be recreated by observations. Replaced attachments,
future revisions and old callbacks cannot write a new generation.

| Operation | Commit boundary and partial-failure rule |
| --- | --- |
| New Add | Reserve namespace; stage closed gates; insert record/catalog/intent; activate/bind after commit |
| Duplicate Add | Only explicit metadata/permanence; never Resume/reset selection/FIFO |
| Pause | Fail-closed quiesce allowed before commit; only committed intent survives reopen |
| Resume | Stage saved selection/FIFO, commit enabled + authorized retry, probe/write-generation/latch persistence, apply/bind |
| Priority | Commit base selection; failed apply leaves the committed revision pending |
| Metadata/permanence | Narrow explicit writes; sync cannot reverse them |
| Delete | Persist keep/payload mode before Drop/unlink; preserve pending mode/manifest on later failure |
| Migration | Journal/reserve before copy; verify/commit before source cleanup; retain source on failure |
| Verification | Persist its own safety veto before work; success removes only that veto |

SQLite and library/filesystem are not one transaction. Failed commit never causes
automatic Allow. Committed intent whose apply fails remains authoritative with
applied zero/reconcile pending and bounded runtime-only `control.apply_failure`
stage code, never raw path/error. Full explicit Pause/Resume retries once; no
unbounded worker/retry queue. Incremental metadata/policy/selection cannot silently
acknowledge an earlier failed run/selection command. Reopen replays desired intent
and safety keys, not old Downloading/applied revision/operation history.

Explicit Resume alone atomically removes retryable legacy/missing/metadata/
selection keys with enabled intent. Verification/workflows/disk retain separate
authorities. Add-only metadata/selection/missing safety evidence may cross a command
revision on the SAME current attachment; verifier removal stays exact-fenced.
Pre-binding faults carry at binding; failed new-record commands persist faults
before returning rather than waiting for a sampler. Failed SQLite writes remain
errors with closed live gates, not a promise of crash durability.

Restore precedence: paused intent, disk, missing, metadata/selection, legacy/
unknown, verification, otherwise Queued. Display precedence removes no keys. Any
activation-block restore is offline, even if disk dominates the displayed phase.
Successful verification never enables networking itself. Read-time physical absence
requires previously current-open verified expectation and both final/part absent;
ordinary incomplete, metadata pending, hash mismatch or a part file is not absence.
No periodic filesystem scan is introduced. Explicit repair resets that expectation.

## Coordinator and shared resources

Repository has no Lifecycle mutex. One application coordinator is created with
TorrentUseCase; SyncWorker receives that same usecase/coordinator. Hash keys
serialize DB-only and live records; root keys serialize physical aliases. Resolve
mapping, acquire sorted/deduplicated keys without a registry mutex, revalidate
mapping, then execute. Active AND waiting references keep a key's lock identity
stable across delete/re-add; cancellation releases waiter/prefix references.
Commands also capture existing record IDs before waiting and revalidate after
acquisition: an old waiter cannot gain authority over a same-hash replacement.

Add parses/re-encodes input before coordination. It first durably reserves
insert-only mappings/aliases, then reacquires the fully resolved key set and
rechecks conflicts before engine/intent effects. An asynchronous alias cannot
redirect an Add to an unowned root lock. SQLite conditional alias writes never
replace a different root. DB-only storage initialization has a short dedicated
mutex, uses insert-only defaults, and never waits on record keys or long I/O.

Lock order: ordered command handles -> Engine admission/map/stream/torrent/library
suffixes (`engine/CONCURRENCY.md`). Registry mutex protects references only. Policy
mutex protects temporary limit/lease counters/debounce data only; no SQLite,
engine or I/O while held. Transfer release carries record ID and cannot consume a
re-added record's lease. Leases outlive command handles, not hold them on network
I/O. Released debounce/identity entries are pruned by cleanup.

Cleanup TryLocks its pass and candidate keys: Add never waits for a cleanup pass or
another unordered object's command. Recheck FIFO/policy/leases under candidate
reservation before durable deletion; an acquired eviction reservation wins before
a later limit change, transfer or MakePermanent. Protected/busy oldest candidates
are safely deferred, not skipped destructively. No background worker per retry.

Migration holds a distinct work ticket across copy/hash, releases command keys
while durable moving/journal vetoes conflicting Add/Resume/transfer/Delete, then
reacquires keys and validates record identity at finalization. Pause may change
desired run; final restore rereads it, never restores an old command snapshot.
Keep-data deletion is vetoed by an unfinished migration manifest too.

BEGIN IMMEDIATE reserves SQLite's one writer before command reads; independent
writers cannot invalidate a deferred read->write upgrade. WAL read snapshots remain
deferred. Transactions contain no library/filesystem/network work. No parallel
SQLite commits or bypass of library locks is promised.

Engine context closes coordinator admission/cancels waiters; an owned task joins
active handles without registry/record/engine locks before resources close. Late
commands are rejected BEFORE creating new intent (including deletion). HTTP action
and priority waits propagate request cancellation; read preparation uses its caller
context. A syscall already running need not be interruptible.

## Read models and publication order

REST single/list, SSE and TorrServer share the validated projection. Each attempt
reads a durable global/per-record version, materializes one SQLite snapshot,
captures runtime pointer/policy/binding stamp and owned DTOs, then rechecks the
compact durable version and runtime stamp. At most two attempts;
continued churn returns `ErrSnapshotConflict`, never guessed/empty success. REST
list/files return 409; SSE retains its last publication. There is no second full materialization/DeepEqual. Relevant intent, identity,
catalog, storage and safety changes advance durable versions; telemetry updates
do not. Speed/peers/bytes/activity are sampled, not readiness authority.

DB metadata/base priorities win over runtime. Engine-only records are omitted;
different record IDs/invalid applied revisions fall back to detached durable
projection with applied zero/no activity. `activity` independently reports session
streams, direct readers, preloads, debounce and verification; descriptors and
accounting sessions can overlap and must not be summed as viewers. HEAD adds none.

REST list and command receipts expose `X-Hub-Snapshot`; CORS exposes that header.
SSE uses `id: epoch/decimal-sequence` while retaining an array `data:` body. Empty
lists have a version too. Allocate snapshot sequence BEFORE capture/encoding;
command acknowledgment retires earlier captures, not their delayed publication
time. Latest-only mailboxes atomically pair version/data; JSON is outside handles.

Vue uses one constant-size list/receipt watermark, not browser time or a growing
tombstone map. Decimal sequences compare losslessly within one epoch. Old empty
lists cannot erase re-add; old lists cannot resurrect deletion. New unsolicited
SSE epochs request a fresh ordered REST handshake; delayed retired commands cannot
switch the browser back. Record identity changes clear selection/pin/file caches;
late command acknowledgments cannot mutate a different current record. Legacy
timestamp barriers work only before version negotiation. Closed EventSource
callbacks cannot publish, and async file/piece/list requests retain request guards.

## Phase 40 command and publication boundaries

Startup restores persisted records in FIFO order before temporary cleanup and the
first SyncWorker publication. HTTP listens during this work, but torrent operations
return 503/Retry-After until initialization finishes. Echo, static assets, logs and
SSE subscriptions remain available without accessing runtime state. Pending SSE
clients receive the first actual snapshot; no synthetic empty list or successful
mutation is emitted during restore. Per-record failures remain logged and returned
to main, which preserves the existing policy of serving successfully restored and
DB-only records. Signal cancellation reaches DB reads and record waits; the startup
owner joins work before closing engine/storage/SQLite.

`runIntentCommand` / `applyCommittedIntent` own commit/apply/bind failure handling.
Duplicate Add commits metadata/permanence together, with one revision. A semantic
no-op does not advance it and cannot acknowledge an earlier failed apply. Full
explicit reconciliation remains necessary after a failed incremental command.

Catalog format and SHA-256 version come from validated immutable layout. V2 offsets
include alignment gaps independently of payload size. Unknown legacy metadata stays
unknown. Unchanged catalogs skip the writer reservation; observations update only
changed file bytes, while sequence/provenance still advance transactionally.

SyncWorker publishes independently of record locks and observation commits. Cached
runtime telemetry is marked `snapshot_freshness: sampled`; current durable intent
and identity always win. Two persistence workers consume at most 128 coalesced hash
jobs, each with record/runtime fencing and a two-second application context. Busy
record keys are retried on the next sample; old work cannot attach to a replacement.
SQLite writer waits use 50 ms busy intervals and cancellation on a dedicated
connection, restoring the normal 5000 ms timeout before returning it to the pool.

REST/SSE `?view=compact` omits catalogs and opaque data. Full responses remain the
default. Full/compact publications retain at most 16 MiB/2 MiB; oversized publication
is rejected and logged, keeping the previous snapshot. At most 128 subscribers each
retain one shared immutable latest buffer. GUI fetches files only for the selected
record and validates `X-Hub-Details` against record/catalog/desired revision.
Confirmed priority overrides end at the first newer authoritative snapshot, even
if another client has changed the priority again. Receipt generations fence older
in-flight details responses as well as lists. Neither details nor snapshots grant
payload readiness.
