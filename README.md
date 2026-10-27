# Torrent-Stream-Hub

Torrent-Stream-Hub is a hybrid torrent client and HTTP streaming server for local networks. It combines regular background torrent downloading and seeding with sequential file loading for real-time playback over HTTP with `Range` request support.

The service is designed for home LAN usage, including low-power devices such as NAS boxes. It exposes a TorrServer-compatible API for Smart TV applications such as Lampa and Num, and also provides its own Web GUI built with Vue 3.

## Features

- Add and manage torrents through a Web GUI or API.
- Stream torrent files over HTTP while data is being downloaded sequentially.
- Support media players that use HTTP `Range` requests.
- Persist torrent state in SQLite.
- Use WAL mode for database reliability.
- Run as a single binary or as a Docker container.
- Publish a LAN-friendly API with permissive CORS.

## Storage Layout, Upgrade And Recovery

New torrents use `HUB_DOWNLOAD_DIR/payload/<readable folder>/`. SQLite stores
the actual root, layout version, metadata prefix, mapping revision and storage
owner; hybrid v1/v2 aliases share that owner and directory. Multi-file torrents
do not repeat their outer metadata folder. Existing `payload/<stable-infohash>`
directories remain valid and are not renamed by a database upgrade. Do not move payload
directories or edit mappings while the service is running. Keep the database,
metainfo and other service files outside the payload namespace.

The Add dialog accepts an optional folder name; otherwise validated metainfo
supplies it. Names use Unicode NFC, sanitized unsafe characters/Windows reserved
names and a 180 UTF-8 byte limit. Case-insensitive collisions in SQLite or on disk
receive `(2)`, `(3)`, etc.; the chosen path persists across restarts. Changing the
display title does not rename files. Magnets have no writable payload root until
metadata and reservation are ready; their display name is only a hint.

The storage panel previews an explicit move/conversion, including source, target,
rename/copy method and required/free space. Apply rechecks those facts, drains
transfers and journals publication before changing the mapping. Cross-filesystem
moves use a verified copy; same-filesystem directory moves use native no-replace
rename when the internal layout is unchanged. Cancellation is accepted only
before publication. Interrupted moves retain claims and resume from their journal.

To connect existing data, analyze a root-relative directory, review matching pieces,
then apply. Analysis is read-only; Apply checks it again, protects the former data
and leaves the torrent paused. Neither fingerprints nor offline verification allow
a new runtime to skip hash checks. Ordinary torrent deletion retains old sources
and backups protected by recovery ownership.

`POST /api/v1/torrent/{hash}/storage` accepts `plan_move`, `apply_move`,
`analyze_location`, `apply_location`, `retry_storage`, `cancel_storage` and
`reconcile_storage`. Send `record_id`, `storage_id`, string `mapping_revision`,
and string operation `version` for apply/retry/cancel. Plans accept `folder_name`;
analysis accepts `path` and optional `strip_prefix`. Accepted commands return 202;
stale/conflicting identities return 409, queue saturation 429 with `Retry-After`,
and shutdown 503. See the [contract](tasks/phase42-storage-contract.md).

Existing installations require an explicit storage check in the panel to enroll
the volume; empty installations enroll automatically. Include `.tsh-volume` in
the coordinated backup with SQLite. A missing/different registered marker blocks
writes/cleanup and is never recreated automatically. Restore the expected mount
and matching marker from backup, then check storage again. Enrollment remains
possible for interrupted deletion/recovery without clearing its journal or block;
retry/cancel stays a separate action. Reconcile leaves the record paused. Check
remaining blocked records after enrollment, or restart for a fresh inventory.

Inventory distinguishes an expected file that disappeared from one never created;
unknown legacy evidence stays unknown. Explicit Resume authorizes missing-file
repair only after volume validation. Tokens and live device/volume identities
detect ordinary mount loss/replacement but cannot prove a disk was not cloned with
its marker. A partial marker after interrupted initial enrollment fails closed:
while stopped, restore the coordinated backup or inspect that enrollment. Do not
replace an enrolled token with a newly generated one.

Payload files use their original names and extensions from the first downloaded
byte (for example, `movie.mkv`); the application no longer appends `.part` or
renames files when completion changes. A normal filename does not imply a complete
file: streaming/download readiness still requires current-open piece verification.
Existing legacy `.part` files are imported under their metadata names before the
runtime opens storage, using an atomic no-replace rename without copying the data.
Incomplete bytes are retained for resume. If both names exist, opening that
torrent fails with a conflict and preserves both files; it never guesses which
copy to overwrite. Interrupted imports resume on the next open. A filename that
actually ends in `.part` in the torrent metadata is kept as-is.

Before upgrading an installation using the old shared layout:

1. Stop the service cleanly. Back up the **entire download and config directories**,
   including SQLite, metainfo, completion data and any remaining `-wal`/`-shm`
   files as one consistent, offline snapshot.
2. Provide additional free space for a copy of the largest torrent being migrated,
   plus the configured `HUB_MIN_FREE_SPACE_GB` reserve. Migration copies files
   sequentially, preserving original files until the mapping commit. It does not
   use an in-place rename optimization.
3. Start the new binary and explicitly check/enroll the original storage volume.
   A background worker inventories ownership, journals the
   copy, checks its digest and performs an offline torrent hash pass. Existing
   partial or corrupt pieces remain incomplete and must be downloaded again;
   verification does not turn them into trusted data. Migration does not change
   temporary/permanent policy, FIFO position or saved file priorities.
4. Monitor `storage_state` / `storage_error` in the torrent list/API and the logs.
   `legacy` means queued for migration; `moving` covers copying and offline
   verification. Logs identify `preflight`, `copy`, `verification`, and `cleanup`
   stages, with file counts during copy/cleanup. The worker processes torrents
   sequentially, so a large catalog can delay later entries, especially on HDDs.
   Saved run intent and legacy progress remain pending until migration and
   runtime verification finish; they do not imply that metainfo was lost.
   `legacy`, `moving` and `blocked` entries cannot write or stream. Deletion is
   refused while a migration journal is pending or ownership cannot be proven.
   Unaffected ready entries remain usable.
   Interrupted migrations retain their journal and retry automatically (including
   after restart); do not delete journal-owned copies or temporary files manually.
   Fingerprints are committed in batches of up to 64 files before those copies
   are published. Existing journals remain compatible; after interruption,
   published targets are revalidated even if their progress flags were not saved.

Shared legacy paths, an occupied target, missing original metainfo, symlinks,
hardlinks or ambiguous aliases cause a conservative block rather than choosing
an owner or overwriting files. Stop the service and make another backup before
repairing anything. Restore the exact original `.torrent` in the config
`metainfo/<stable-infohash>.torrent` location when it is missing; free disk space
or correct permissions for I/O failures. Restart to retry. For ambiguous ownership
or an occupied target, restore a known-consistent backup or resolve ownership
offline; never merge directories or blindly remove a conflicting target. Preserve
both copies and the database journal if recovery needs manual investigation.

After each startup/reopen, existing payload is hash-checked again with bounded
background concurrency; old completion hints, a final filename, file size and a
saved 100% progress value do not authorize streaming/download/seeding. Verified
pieces can be read while other pieces are still being checked. Direct streaming
and completed downloads require all relevant pieces to be verified. Changes to
inode, size or modification time revoke trust and trigger rechecking when
observed. External writers must remain stopped during verification and transfers:
stat checks cannot detect content changes that preserve every checked attribute,
nor guarantee an immutable file while it is being served.

**Downgrade is not an in-place operation.** Old binaries do not understand the new
mapping. To return to an old version, stop the new service and restore the entire
pre-upgrade download/config snapshot together, then start the old binary. Do not
point an old binary at the migrated database or isolated payload tree.

## Download Priorities And DiskFull Recovery

File priorities (Normal/High/Skip) are persisted user settings, not runtime
statistics. Startup and Resume restore them before enabling payload requests,
including when a magnet is still waiting for metadata. Stream/preload overlays
do not change these saved settings. A paused torrent stays paused after restart.
Use Resume, not a duplicate add, to restore an existing DB-only torrent.

The service checks free space on each payload root's filesystem before admitting
payload downloads, at Resume, and periodically. Falling below
`HUB_MIN_FREE_SPACE_GB`, an unknown free-space measurement, or a payload storage
write error closes the library-level download gate, including reader/stream
requests and queued torrents. An unknown measurement is reported separately from
an actual `ENOSPC`. The block is stored separately in SQLite and survives restart;
freeing space alone does **not** restart downloads.

To recover, free space (or correct the measurement/storage error), then explicitly
Resume each affected torrent. Resume returns HTTP 409 if the disk check still
fails, or HTTP 500 if its database changes cannot be committed; it does not report
a false success. Successful Resume retains file priorities and obeys the background
download limit. `download_block` and the error reason are visible through REST/SSE
and the Web GUI; manually paused torrents are not activated by the disk monitor.

Already verified data can still be streamed/downloaded by the direct disk fast
path and seeded without payload writes. A stream waiting for missing data fails
with a diagnostic error when blocked; it must be reopened after recovery. The
service does not reset QoS reference counts/debounce timers to remove a disk block.

The gate cancels pending/new payload requests; it cannot roll back a system write
already in progress. Such in-flight operations may finish or fail. Writer errors
also close admission synchronously in the torrent library, before the asynchronous
application observer persists the reason; automatic callbacks cannot erase that
error latch. SQLite, completion-cache and log writes themselves can fail on a full
or read-only volume. Persistence is not guaranteed when the underlying filesystem
cannot commit it: failure keeps the runtime gate closed, and startup rechecks disk
space before any payload admission. Keep backups and leave free-space headroom.

## Concurrent Commands, Shutdown And Deletion Recovery

HTTP starts before persisted torrents are restored. During restoration, the web
page, `GET /echo`, `/api/v1/logs` and `/api/v1/logs/status` remain available.
Torrent operations return `503 Service Unavailable` with `Retry-After: 1` until
restoration, startup temporary cleanup and the initial snapshot finish. SSE
subscriptions can connect immediately and receive that first snapshot when it is
published; connecting does not publish an empty torrent list. The UI opened during
startup receives updates without a page reload. Logs include the storage-init
stage, torrent count, each hash and its duration, cleanup and snapshot stages.
SIGINT/SIGTERM cancels startup as well as normal workers; engine/storage and SQLite
stay open until accepted work finishes. A blocked OS syscall can still delay exit.

Pause, Resume, scheduler activation and stream priority reconcile share an engine
admission lock. A delayed metadata/recheck/discovery/stream callback cannot undo
a completed Pause or act on a deleted torrent generation. Stream overlays combine
ranges across files, including shared boundary pieces. Seek debounce and stream
reference counting remain active; old timer/session tokens cannot clear a new
session or a re-added torrent with the same hash.

The background scheduler reserves slots atomically, including metadata-pending
downloads and active rechecks. A positive `HUB_MAX_ACTIVE_DOWNLOADS` limits
background downloads; zero does not promote background downloads. Stream-driven
queued requests remain separate and do not enable the whole background selection.
Queue ordering and selected-files completion policy have not changed.

Shutdown rejects new work, cancels torrent/reader contexts and timers, joins
accepted engine tasks, and then closes library/storage resources. Cancellation
cannot forcibly interrupt an already running filesystem syscall or an arbitrary
blocking input reader supplied to an add operation. Internal ownership and lock
ordering are documented in `internal/engine/CONCURRENCY.md`.

Deletion records durable intent before stopping the runtime. With
`delete_files=true`, a verified manifest is committed before unlink; with
`delete_files=false`, mode `keep` forbids payload removal, including on retries.
If deletion fails, REST/SSE and both GUI layouts show `deletion_mode` and
`deletion_error`. Resume, transfers and re-add conflict with pending deletion;
restart does not restore its download. Fix the reported problem and retry Delete
**with the same delete-files mode**. A different mode returns a conflict, not an
implicit escalation to destructive deletion. Missing files are safe on retry.

Manual pending deletions are not automatically retried at startup; temporary
payload cleanup retains its existing periodic retry policy. An unsafe/incomplete
manifest is never guessed or treated as proof of an empty payload. Deletion errors
return failure, not success with hidden pending work. If recording the error also
fails, the already committed intent remains the recovery authority.

Final database deletion is transactional. Payload deletion removes unused mapping
aliases with their root; aliases still referenced by another torrent are retained.
Keep-data deletion deliberately retains namespace reservations so a later re-add
can reopen the same isolated data. Cached metainfo and application-owned files
are not payload and are not removed. Schema upgrades import existing cleanup
manifests as pending `payload` deletion; back up SQLite and payload before upgrades.

## Progress, Recheck And Paused Reads

`downloaded`/`progress` describe the currently verified payload observation, not
all bytes ever received. They may decrease after corruption, replacement or
recheck. REST/SSE expose `progress_source`: `verified` for a live observation,
`checking` during verification, `unknown` when it is unavailable, and
`last_known`/`legacy` for database estimates. Stored estimates never authorize
reading, upload or cache readiness. `legacy_downloaded` preserves old observations
without pretending they are cumulative network traffic. Current network counters
remain in BT health diagnostics. `progress_observed_at_ns` orders observations;
it is diagnostic wall-clock time, not a readiness token.

Cached piece length/count and file offsets carry `metadata_known`/`offset_known`.
The UI labels last-known progress and does not invent a piece map for unknown
offsets. Empty files do not produce NaN percentages.

`selection` separately reports selected size, current verified bytes, optional
percentage and complete status: one ready file of two equal files can be selected
100% while whole progress is 50%. Empty selection has no percentage and does not
imply seeding. File `status` separates presence/freshness, verification, current
verified bytes, last-known bytes/time/source, effective priority and direct-read
readiness. `metadata_path` and `status.storage_path` show logical/physical paths.
A ready skipped file may still be read locally. Compact SSE omits the full catalog;
details are fenced by record/catalog/intent/mapping/attachment.

Verification reports checked bytes (including unsuccessful hashes), queued and
active pieces. Reader ranges precede readahead, selected files and background work;
every eight priority grants allow an oldest background turn. Main/offline clients
share two active hash read/compute tasks, one per live filesystem, and 64 pending
leases within existing client/torrent slot limits. Completion flushes may overlap
the next hash. Seek never interrupts a running filesystem syscall. Persistent
completion remains a hint, separate from validated durable resume checkpoints.
With default fast resume, legacy `verified` progress means usable hash-verified
or trusted-restored data, not necessarily hashing in this attachment. See `resume`
below; `verification.checked_bytes` counts actual checks only.

`POST /api/v1/torrent/{hash}/action` with `{"action":"recheck"}` returns **202**
with an engine-session operation ID and status; list/SSE snapshots include
`recheck.status` and `recheck.error`. Repeated requests during a running operation
join it. No exact percentage is invented. A completed operation does not imply
every piece is valid: inspect verified progress. Operation history is runtime-only
and does not survive restart as a falsely successful result. Conflicting active
transfers, missing metadata and deletion/migration return 409; missing torrents
return 404. A failed check keeps readiness revoked until a successful local check;
Resume cannot bypass it. Pause is not undone by a late result.

The verification safety block is stored independently of operation history and
survives restart. An interrupted check, or failure to persist its successful
result, does not reopen read/upload/download gates. Rechecking successfully clears
only that verification block, not a disk latch or deletion/migration restriction.
Background snapshots do not change saved Pause/Resume intent or file selection;
startup uses that intent instead of a previously observed Downloading/Seeding
phase. New additions stay payload/upload-disabled until their record and catalog
have committed. Phase 39's broader state/coordinator refactor remains in progress.

Torrent REST/SSE views include `control`: record identity, desired run/revision,
live applied revision/attachment, activation blocks and reconcile pending/reason.
Revisions are decimal strings (not JavaScript numbers). A DB-only record reports
applied revision `"0"`; an older process's saved observation is not acknowledgment
by a live runtime. The UI displays pending apply separately from operational state.
Pending diagnostics do not promise an automatic retry. Explicit Resume may retry
legacy errors/missing payload and authorize network repair, but never bypasses a
verification block or certifies existing bytes. DiskFull still requires the
existing explicit Resume disk/write-generation checks and persistence.

When a command partially fails against a live runtime, `control.apply_failure`
identifies its failed commit/stage/apply/bind step without exposing raw paths.
Transfer admission stays closed, and applied revision becomes `"0"` until an
explicit full Pause/Resume successfully reconciles the saved intent. Metadata and
snapshot delivery cannot silently clear this failure. This diagnostic is runtime
only: restart replays SQLite intent and durable safety blocks, not error history.

Failed or cancelled verification retains its own activation veto; it is not an
`Invalid torrent` diagnosis. A successful local retry clears only that verification
error/block, preserving Pause, disk latch and unrelated errors. Verification owns
no background download slot, and a late completion cannot finalize a newer check.

DB-only REST/SSE and startup restore project state from saved intent and typed
activation keys, not a historical Downloading/Seeding snapshot. Detailed block
reasons remain in `control.activation_blocks`; legacy activation errors display
`Activation blocked` so diagnostic text cannot grant verification-recovery rights.
Blocked restore stays offline even when a coexisting disk latch displays DiskFull.

Staging a full Resume closes download/upload and reports applied revision `"0"`
until full application and binding succeed; this is not itself a committed Pause.
Runtime metadata/selection faults also revoke admission and acknowledgment. Invalid
file selection is diagnosed separately from invalid metainfo. Typed durable keys
`metadata_persistence` and `invalid_selection` prevent automatic activation after
restart; explicit Resume authorizes retry after preparation of the saved settings.
Verification never removes those keys. Schema migration 0010 preserves the existing
0009 keys, including databases created during phase 39 development.

`missing_files` is raised on read-time confirmation that a previously verified
current-open payload disappeared from its metadata path. A leftover `.part`
neighbor does not replace that path after import. Ordinary
incomplete files, metadata pending and hash mismatch do not prove disappearance.
No background filesystem scan is added. Explicit Resume authorizes repair, not
readiness. Failed SQLite persistence returns an error and keeps live gates closed;
like other failed commits it cannot promise durability across power loss.

`activity` reports live `streaming`, session `streams`, `direct_readers`, `preloads`,
`debouncing` and `verifying` independently of operational phase and saved intent.
A disk reader and its accounting session can belong to the same HTTP request, so
their counters must not be summed as distinct viewers. HEAD creates neither.
Detached views report no activity. Engine run/operation/slot/fault authorities are
independent; phase is a projection, not scheduler/gate authority. Scheduler probes
and payload preparation run outside global admission with generation/epoch/slot
tickets. REST/SSE assembly validates durable snapshots against runtime identity
and policy/binding, retries once, then reports a snapshot conflict (HTTP 409 for
list/files) rather than publishing guessed/empty state. Live speed/peer/activity
counters are still sampled telemetry. Commands, sync, cleanup and migration share
a keyed hash/root coordinator; repository no longer owns a global Lifecycle mutex.
Registration, Drop/reader close, Resume/restore cache/probe and absence checks use
reserved, generation-validated I/O outside global admission. Transfer leases carry
record ID; eviction rechecks FIFO/policy/protection before destructive work. SQLite
write transactions use `BEGIN IMMEDIATE`, while WAL read snapshots stay deferred.

REST list and command receipts expose `X-Hub-Snapshot` (also exposed through CORS).
SSE retains its array payload and adds `id: epoch/decimal-sequence`, even for empty
lists. Vue compares lossless server sequences within an epoch, retires pre-command
publications and confirms process changes through fresh REST; old lists/empty
lists/command responses cannot resurrect deletion or erase re-add. These versions
are not browser timestamps or persistent audit history. Shutdown closes task and
command admission: late commands fail before creating new intent, while previously
committed deletion/migration journals remain recoverable.

After restart, Paused torrents with cached hash-matching metainfo reopen with
networking, upload and payload download disabled. A local hash pass can establish
direct read readiness for streaming/HEAD/Range/download without implicit Resume.
Missing/corrupt/incomplete data never causes automatic network repair. Missing
cached metainfo is an explicit conflict on read/recheck, not a metadata fetch.
Local verification may take time on slow disks. Resume explicitly enables normal
discovery again; pausing an already online runtime is not a promise to cancel all
existing discovery operations.

Priority commands require an explicit enum value (-1/0/1). Invalid input returns
400, missing torrent/file 404, and unknown file metadata or storage conflicts 409.
Known persisted file selections can be changed while metadata is pending; an
unknown index or bulk operation without a file list is not silently accepted.
Successful commands also return `command_at_ns`; views carry `snapshot_at_ns`.
The GUI ignores delayed pre-command views instead of undoing a confirmed action.
Legacy timestamp ordering assumes a stable server wall clock. Current clients use
`X-Hub-Snapshot` sequences instead; neither mechanism is a readiness or access token.

Background queue order uses the existing persisted FIFO sequence. Resume and
duplicate add do not move it; deletion/re-add creates a new position. Skip All or
a completed selected subset releases the background slot. A completed subset is
reported separately and does not turn an incomplete torrent into Seeding or
100% payload completion. Selecting more files makes it eligible again, subject
to Pause/DiskFull and the background limit. Speed sampling runs once per second;
API/polling frequency no longer changes its baseline or watchdog measurements.

## Preload And SSE Limits

Preload joins one warmup per torrent generation/file. Limits are fixed at **4
active warmups**, **128 retained states**, a **2-minute timeout**, and **10-minute
TTL** (swept every minute without polling the key). Saturation returns 429;
shutdown rejects new work with 503. Readers/transfer leases and capacity tokens
are released once even after failures/cancellation. Active transfer leases still
conflict with manual Delete; engine generation cancellation and shutdown stop
the warmup. `preload_status`/`preload_error` distinguish failure from completion.
An HTTP disconnect does not cancel a shared accepted warmup.

SSE keeps the latest snapshot rather than ten stale updates. Writes/flushes are
checked, each write has a 10-second deadline, and idle connections get a heartbeat
every 15 seconds. Writers/middleware without deadline/flushing support receive
503; preserve ResponseController support in reverse-proxy integrations. Service
shutdown closes subscriptions before waiting for HTTP handlers. These deadlines
are specific to SSE and do not impose a short video WriteTimeout.

The emulated cache window uses `HUB_STREAM_CACHE_SIZE` for both partial and fully
verified files. Zero disables the reported window; negative values normalize to
zero. Full-file readiness remains independent of that fake limit. HTTP input and remote-metainfo budgets are described below.

## Storage Publication And SQLite Completion (Phase 40)

The production completion cache uses the existing SQLite WAL database. Migrations
0011–0013 add explicit v1/v2/hybrid catalog layout, disposable completion hints and
read-model versions. V2 aligned offsets are stored independently of payload size;
unknown legacy layout is not guessed. Old Bolt completion files are neither opened
nor deleted; retain them with backups if an offline rollback is needed. Hints never
replace verification after reopen. The cache retains 4096 entries, up to 1024 queued
writes and one detached batch of at most 1024. Overload drops hints, counted in
`piece_completion_dropped`; persistence errors appear in engine diagnostics.

Legacy `.part` import flushes the old file and uses one native atomic
**no-replace** rename within the same directory: Linux `renameat2`, macOS
`renameatx_np`, Windows handle-relative rename. Filesystems must implement that
primitive; unsupported filesystems fail the operation without falling back to
Link+Remove or overwriting an existing name. Linux/macOS sync the parent directory;
Windows has no portable directory fsync in this implementation. Atomic visibility
is not a claim of survival of every hardware/power failure.

A legacy interrupted Link+Remove may leave `.part` and final names for one inode.
Both are preserved and rejected as ambiguous hardlinks. Stop the service, back up
both names and metadata, and establish ownership of **all** links before manually
removing a redundant name or restoring a consistent backup. The service cannot
prove ownership of an arbitrary old hardlink and never automatically unlinks it.
Restart and verify the payload after recovery. No new import creates that
intermediate state. Downgrade still requires restoring the entire offline backup.

## HTTP Resource Limits And Remote Metainfo

| Resource | Limit / behavior |
| --- | --- |
| JSON/form/control body, including `/cache` | 1 MiB, 30 seconds, at most 32 simultaneous requests retaining a parsed body |
| Multipart `.torrent` upload | One file up to 32 MiB, 34 MiB total envelope, 16 parts, 64 KiB total fields, 30 seconds |
| Upload and remote-metainfo jobs | Two shared jobs per service; managed upload files total at most 64 MiB while running |
| HTTP metainfo fetch | 32 MiB, five redirects, 60 seconds for the entire operation |
| Video/file/archive responses | `HUB_MAX_ACTIVE_STREAMS` shared slots, default 4; reserve before preparation |

Oversize returns **413**, body/fetch timeout **408**, occupied budget **429**,
forbidden source **403**, cancellation/shutdown **503**, upload filesystem failure
**507**. CORS `*` applies to errors too. Failed multipart parsing/cancellation removes
its temporary file. A killed process can leave an orphan spool file: stop the
service before removing only `metainfo-*.tmp` from its dedicated spool directory.
No global video/SSE write timeout is imposed. HEAD holds a short deletion-safe
lease but consumes no transfer slot and creates no playback debounce. A response
releases its slot immediately; the separate ten-second seek debounce remains.

Remote HTTP/HTTPS metainfo resolution (TorrServer `POST /torrents`) rejects
loopback, private, link-local, unspecified, multicast and mapped private addresses
by default. Every redirect and every DNS answer is checked. Connections dial a
validated IP pinned for that operation; HTTPS still checks the original hostname
and certificate. Environment HTTP proxies are intentionally ignored. Userinfo in
URLs is rejected, sensitive headers are not forwarded, and hostname input is ASCII
(use punycode for IDNs). A magnet redirect changes the result type and does not
cause a network request to an arbitrary scheme.

For an intended LAN source, configure a narrow allowlist, for example
`HUB_METAINFO_ALLOWLIST=nas.lan:8081,192.168.50.10/32`. Host rules authorize the exact
hostname/port; CIDR rules authorize that address range on any port. Each redirect
must independently pass policy. An allowlist does not disable TLS validation or
permit unspecified/multicast destinations.

## Discovery And Supported Diagnostics

Known private torrents keep their original trackers and never receive public
retrackers, DHT or PEX. Before magnet metadata arrives, the private flag is unknown:
only supplied trackers and ordinary DHT/PEX are available; extra retrackers wait
for confirmed public metadata. Discovering private metadata cancels public DHT
work and discards prior peer connections/reserve. It cannot undo an earlier public
announce. For privacy from the first request, pass `"private_safe": true` in the
native/TorrServer Add JSON, or `x.hub-private=1` in a magnet. This disables DHT/PEX
and public retrackers from admission and is preserved on restart. Changing an
existing public record to private-safe requires deletion/re-add, not duplicate Add.
Explicit trackers in such an Add must themselves be suitable for the private swarm.
With DHT disabled, an unknown magnet needs a supplied tracker/peer for metadata.
For deferred public magnets, `replace` supplies additional retrackers after
metadata while retaining the original bootstrap trackers; known public `.torrent`
inputs apply replacement before admission.

Upload burst is at least 64 KiB even with a low sustained B/s limit, allowing normal
protocol blocks while rate reservations continue to enforce average speed. Swarm
boost expires (default 300 seconds); repeating it extends the deadline, and expiry
restores the previous connection limit. Diagnostics display the actual boost until
the engine restores that limit.

Hard refresh/client recycle operations are not supported. Legacy
`HUB_BT_SWARM_HARD_REFRESH_*`, `HUB_BT_SWARM_AUTO_HARD_REFRESH_ENABLED`,
`HUB_BT_CLIENT_RECYCLE_*`, `HUB_BT_SWARM_DEGRADATION_EPISODE_TTL_SEC`,
`HUB_BT_SWARM_RECOVERY_GRACE_SEC`, `HUB_BT_BENCHMARK_MODE` and corresponding CLI flags
remain parseable for upgrade compatibility, but explicit use logs **deprecated,
has no effect**. Remove them from deployments. Their parsers will only be removed
in a separate versioned configuration change with migration notes. Use the
supported Pause/Resume/Recheck/priority actions and diagnostic Reload.

The following environment variables can be **deleted without replacement** (also
remove their corresponding `--bt-*` flags). Setting them to `false` still counts
as explicit use and produces the warning:

```text
HUB_BT_BENCHMARK_MODE
HUB_BT_SWARM_HARD_REFRESH_ENABLED
HUB_BT_SWARM_AUTO_HARD_REFRESH_ENABLED
HUB_BT_SWARM_HARD_REFRESH_COOLDOWN_SEC
HUB_BT_SWARM_HARD_REFRESH_AFTER_SOFT_FAILS
HUB_BT_SWARM_HARD_REFRESH_MIN_TORRENT_AGE_SEC
HUB_BT_SWARM_DEGRADATION_EPISODE_TTL_SEC
HUB_BT_SWARM_RECOVERY_GRACE_SEC
HUB_BT_CLIENT_RECYCLE_ENABLED
HUB_BT_CLIENT_RECYCLE_COOLDOWN_SEC
HUB_BT_CLIENT_RECYCLE_AFTER_HARD_FAILS
HUB_BT_CLIENT_RECYCLE_AFTER_SOFT_FAILS
HUB_BT_CLIENT_RECYCLE_MIN_TORRENT_AGE_SEC
HUB_BT_CLIENT_RECYCLE_MIN_TORRENTS
HUB_BT_CLIENT_RECYCLE_MAX_PER_HOUR
```

These settings are not responsible for legacy filename conflicts or pending
deletion. Current Compose and `make run-local` examples do not need them. Removing
them from this repository does not edit an existing container's environment.

## Storage recovery and migration diagnostics

`storage_state` describes **layout**, not successful runtime attachment or verified
data. The optional `storage_recovery` object is shared by full/compact list, torrent
details, SSE and storage action errors. It has stable `reason`/`actions` codes,
`record_id`, decimal-string operation `version`, separate `runtime` and
`data_readiness`, deletion status, candidate analysis and stage/byte progress.
The legacy `error` field remains supported; clients must not parse Go error text.
Details: `GET /api/v1/torrent/{hash}`. [Wire contract](tasks/phase41-recovery-contract.md).

Recovery commands use the existing `POST /api/v1/torrent/{hash}/action` endpoint:

```json
{"action":"cancel_deletion","record_id":"<current record_id>","version":"<current version>"}
```

| Action | Meaning |
| --- | --- |
| `retry_deletion` | Continue the saved mode and manifest; never switch keep/payload implicitly. |
| `cancel_deletion` | Commit cancellation, wait for the current worker/syscall, then stop further deletion. |
| `forget_record` | After completed cancellation, remove only the record and keep remaining files. |
| `retry_ownership` | Wake the normal sequential migration worker for a fresh ownership check. |
| `analyze_legacy` | Run bounded, local-only analysis of the next final/`.part` conflict. |
| `apply_legacy` | Include `"choice":"final"` or `"choice":"part"`; retain the other whole file and verify. |
| `resume_verify` | Explicitly verify remaining payload locally, then attempt Resume through normal safety gates. |

Heavy analysis/apply/verification and scheduled ownership retries return **202**;
watch REST/SSE for the result, not HTTP completion. Deletion retry/cancel/forget
return 200 only after completion. Requests must use current identity/version;
re-add invalidates previous recovery capabilities. Actions are advertised only
when applicable. A mismatched `delete_files` on an existing deletion returns **409**
with `code=deletion_mode_conflict`, its current mode and recovery actions; it never
silently changes the intent. Other stable reasons include `legacy_file_conflict`,
`deletion_pending`, `deletion_cancelled`, `ownership_catalog_changed`,
`migration_queued`, `migration_active`, `attachment_failed`, `disk_full` and
`recovery_complete`. Unexpected database/I/O errors remain server failures.

**Cancellation cannot restore removed files.** `removed_data` is `none`, `partial`
or `unknown`; old manifests import as `unknown`. Cancellation is durable before
waiting for the worker. Disconnecting the HTTP request or shutting down does not
erase the intent; restart finishes a requested cancellation before runtime restore.
The torrent remains paused, and explicit verification/resume is needed. A blocked
OS syscall still owns its resources until it returns; cancellation is not a claim
that the syscall has been interrupted.

Normal startup/Resume never merges or chooses conflicting files. Explicit analysis
streams candidates and uses the library verifier, including v1 pieces spanning
files and v2/hybrid alignment. Missing neighboring data can make both candidates
inconclusive. Identical files and fully verified candidates can be suggested, but
the GUI still requires an explicit whole-file choice; differing partial pieces are
not merged. Analysis and copied filenames do **not** grant current-open readiness.

Apply records a durable journal before mutation, checks identity/fingerprints and
ownership, moves the alternative with native rooted no-replace publication, then
publishes the selection and verifies. The protected directory is
`<downloads>/.tsh-recovery/<record_id>/<operation-version>/`. It must stay on the
same filesystem as downloads. Unsupported publication, changed/linked candidates,
DiskFull and flush failures stop safely; retry the journal rather than deleting
names by hand. `failure_code` distinguishes candidate changes/missing candidates,
unsafe files, existing targets, unsupported operations, flush, disk-space and I/O
failures. A flush error can follow a successful rename; restart inspects both names.

Protected alternatives and their logical file bytes are shown in the GUI (sparse
or compressed allocation can differ). They are
**never removed by Resume, torrent deletion or temporary cleanup**, and can remain
after removing the torrent record. There is no automatic recovery-backup garbage
collection. Keep them until you have independently verified and backed up the chosen
payload; manage archival/deletion separately while the service is stopped.

Migration remains sequential for HDDs. Progress reports `preflight`, `checksum`,
`copy`, `verification`, `cleanup`, plus recovery stages. Byte counts measure actual
stage reads, including safety re-reads; they are not cumulative download progress.
Unknown totals are omitted, not zero. Copy may include both source reads and
destination validation, so no whole-operation percentage/ETA is promised. Current
file bytes, file counts, queue position, stage start, last byte/file progress and a
separate heartbeat are exposed. The regular two-second snapshot sampler produces
the heartbeat even while the storage worker is blocked; heartbeat never advances
last-progress, reads payload or writes a journal. Publications remain latest-only.
Repeated identical migration failures retain a count and are log-rate-limited.

### Upgrade rehearsal (always on a copy)

1. Stop the old service; save **downloads, config/metainfo and SQLite together**.
   Use a consistent SQLite backup or copy the DB and its WAL/SHM while stopped.
2. Make a separate rehearsal copy, including existing migration/cleanup data and
   `.tsh-recovery` if present. Do not mount the production payload writable in tests.
3. Start the new version against the copy. Migration 0014 imports pending deletion
   without clearing its mode/manifest, and labels its prior unlink progress unknown.
   Migrations 0015–0019 add named mappings/claims, volumes/inventory and file
   projection, preserving old roots and journals. Enroll the rehearsal volume
   explicitly when required; preserve its token if already registered.
4. Inspect REST/SSE: layout, runtime, metadata-known, pending actions and queue.
   Rehearse retry/cancel and candidate analysis/apply; interrupt/restart if needed.
5. Compare candidate/backup bytes, metainfo and records with the saved baseline.
   Recheck payload before streaming/upload. Only then separately plan production
   deployment; this procedure does not authorize modifying the live instance.

**Downgrade:** do not run an older binary on this updated working set. Older
versions do not understand new layouts, cancellation/fencing or recovery journals. Restore
the coordinated pre-upgrade payload/metainfo/SQLite backup instead. Do not merely
delete migration records or downgrade SQLite while keeping mutated filenames.

## Ports

- `8080/tcp` - Web GUI and HTTP API.
- `50007/tcp` - BitTorrent TCP port.
- `50007/udp` - BitTorrent UDP port.

Open `http://<host-ip>:8080` from another device in the same local network after starting the service.

## Run With Docker

The Docker image is published to GitHub Container Registry:

```bash
docker run -d \
  --name torrent-stream-hub \
  --restart unless-stopped \
  -p 8080:8080 \
  -p 50007:50007/tcp \
  -p 50007:50007/udp \
  -v "$PWD/downloads:/downloads" \
  -v "$PWD/config:/config" \
  -e HUB_PORT=8080 \
  -e HUB_TORRENT_PORT=50007 \
  -e HUB_DOWNLOAD_DIR=/downloads \
  -e HUB_DB_PATH=/config/hub.db \
  ghcr.io/ivangolenkov/torrent-stream-hub:latest
```

## Run With Docker Compose

Create `docker-compose.yml`:

```yaml
services:
  torrent-stream-hub:
    image: ghcr.io/ivangolenkov/torrent-stream-hub:latest
    container_name: torrent-stream-hub
    restart: unless-stopped
    ports:
      - "8080:8080"
      - "50007:50007/tcp"
      - "50007:50007/udp"
    volumes:
      - ./downloads:/downloads
      - ./config:/config
    environment:
      HUB_PORT: "8080"
      HUB_TORRENT_PORT: "50007"
      HUB_DOWNLOAD_DIR: /downloads
      HUB_DB_PATH: /config/hub.db
      HUB_MIN_FREE_SPACE_GB: "5"
```

Start it:

```bash
docker compose up -d
```

View logs:

```bash
docker compose logs -f
```

Stop it:

```bash
docker compose down
```

## Run From Binary

Download a binary archive from GitHub Releases for your platform:

- macOS Apple Silicon: `torrent-stream-hub-macos-arm64.tar.gz`
- macOS Intel: `torrent-stream-hub-macos-amd64.tar.gz`
- Windows x86_64: `torrent-stream-hub-windows-amd64.zip`
- Linux x86_64: `torrent-stream-hub-linux-amd64.tar.gz`

Example for Linux or macOS:

```bash
mkdir -p downloads config
tar -xzf torrent-stream-hub-linux-amd64.tar.gz

HUB_PORT=8080 \
HUB_TORRENT_PORT=50007 \
HUB_DOWNLOAD_DIR="$PWD/downloads" \
HUB_DB_PATH="$PWD/config/hub.db" \
./torrent-stream-hub-linux-amd64
```

Example for Windows PowerShell:

```powershell
New-Item -ItemType Directory -Force downloads, config
Expand-Archive .\torrent-stream-hub-windows-amd64.zip -DestinationPath .

$env:HUB_PORT = "8080"
$env:HUB_TORRENT_PORT = "50007"
$env:HUB_DOWNLOAD_DIR = "$PWD\downloads"
$env:HUB_DB_PATH = "$PWD\config\hub.db"
.\torrent-stream-hub-windows-amd64.exe
```

## Configuration

The service can be configured with command-line flags or environment variables. Common options:

| Environment variable | Default | Description |
| --- | --- | --- |
| `HUB_PORT` | `8080` | Web GUI and API port. |
| `HUB_TORRENT_PORT` | `50007` | BitTorrent listen port. |
| `HUB_DOWNLOAD_DIR` | `/downloads` | Directory for downloaded torrent files. |
| `HUB_DB_PATH` | `/config/hub.db` | SQLite database path. |
| `HUB_MAX_ACTIVE_STREAMS` | `4` | Shared HTTP payload response limit; positive integer, includes file/ZIP downloads, excludes HEAD. |
| `HUB_METAINFO_TEMP_DIR` | `http-tmp` beside the database | Managed multipart spool directory (`--metainfo-temp-dir`). |
| `HUB_METAINFO_ALLOWLIST` | empty | Explicit LAN HTTP metainfo destinations: comma-separated host:port or CIDR (`--metainfo-allowlist`). |
| `HUB_MAX_ACTIVE_DOWNLOADS` | `5` | Maximum active background downloads. |
| `HUB_MAX_TEMPORARY_TORRENTS` | `3` | Maximum temporary torrents (`--max-temporary-torrents`); positive integer, restart required. |
| `HUB_MIN_FREE_SPACE_GB` | `5` | Minimum free disk space required for downloading. |
| `HUB_DOWNLOAD_LIMIT` | `0` | Download limit in bytes per second, `0` means unlimited. |
| `HUB_UPLOAD_LIMIT` | `0` | Upload limit in bytes per second, `0` means unlimited. |
| `HUB_STREAM_CACHE_SIZE` | `209715200` | Streaming sliding-window cache size in bytes. |
| `HUB_LOG_LEVEL` | `info` | Log level; set `debug` explicitly for verbose diagnostics. |
| `HUB_LOG_DIR` | `logs` beside the database | Daily log directory (`--log-dir`). |
| `HUB_LOG_RETENTION_DAYS` | `3` | Previous calendar days to keep, in addition to today (`--log-retention-days`). `0` keeps today only; valid range: 0–36500. |
| `HUB_LOG_MAX_FILE_BYTES` | `16777216` | Maximum log segment bytes (`--log-max-file-bytes`); at least 65536. |
| `HUB_LOG_MAX_TOTAL_BYTES` | `134217728` | Managed log disk budget (`--log-max-total-bytes`); at least the file limit. |

BitTorrent-related options are also available, including DHT/PEX/UPnP toggles, retracker configuration and swarm watchdog tuning. See `internal/config/config.go` for the full list.

## Temporary Torrents And Automatic Cleanup

**Warning:** TorrServer-compatible `POST /torrents` with `action: "add"` and `save_to_db: false` **or without `save_to_db`** now adds a temporary torrent. Exceeding the configured limit automatically removes the oldest temporary torrent **and its downloaded files**, including `.part` files. This is FIFO by first successful addition, not by last playback. Repeating an add does not move the torrent to the end of the queue.

Temporary torrents are still saved in SQLite and survive restarts. The default limit is **3**, configurable through `HUB_MAX_TEMPORARY_TORRENTS` or `--max-temporary-torrents` (CLI wins). Only positive integers are accepted; `0` does not disable cleanup. Reducing the limit takes effect at startup after restarting the service.

Permanent torrents do not count toward the limit and are never selected for automatic cleanup. All pre-existing records are migrated as permanent. Native Web GUI/API additions and `/torrent/upload` additions are permanent, and also promote an existing temporary torrent. Repeating TorrServer `add` with `save_to_db: true` promotes the same infohash permanently; a later `false` cannot undo that promotion. There is no conversion back to temporary in this version.

The GUI displays **«Временный»** with **«Сделать постоянным»** in desktop/mobile lists and torrent details. Alternatively:

```bash
curl -X POST 'http://localhost:8080/api/v1/torrent/HASH/action' \
  -H 'Content-Type: application/json' \
  -d '{"action":"make_permanent"}'
```

Promotion is idempotent (`200`), with `404` for a missing torrent and `500` if persistence fails. Native REST lists and SSE include `is_temporary` explicitly.

Cleanup runs after a successful **new** temporary addition, at startup and every five seconds for deferred work. Playback (both torrent-reader and direct-file paths), preload and HTTP downloads protect the oldest torrent; protection includes the ten-second debounce after a transfer ends. While protected, it blocks the FIFO queue: newer torrents are not removed instead, and the limit may temporarily be exceeded. No global lifecycle lock is held during video/file transmission or HTTP-link resolution.

Cleanup errors do not roll back a successful addition or stop the service. A failed deletion stays visible, is logged and retried; the candidate is marked paused when deletion intent is saved. Exact payload paths are finalized after stopping the engine and persisted before unlinking, permitting retry after partial deletion/restart. An interrupted, incomplete manifest requires known metadata before retry, rather than guessing that there were no files. Unsafe paths, symlinks, application storage, shared/aliased payload paths, or inability to establish ownership cause cleanup to stop rather than guess. A metadata-pending torrent can therefore postpone deletion of another torrent's files until non-overlap can be established. Only confirmed payload files and empty parent directories are removed; arbitrary orphan files and metainfo caches are not swept.

Keep the service on a trusted LAN or behind an authenticated proxy: there is no built-in authentication, and CORS `*` intentionally permits Smart TV clients.

### File Safety And Upgrade Notes (Phase 34)

Manual deletion and automatic cleanup use the same exact metadata-derived paths, including `.part`, and remove only empty parent directories. They never guess a second path or recursively delete a directory by torrent name. Active transfers and the ten-second debounce also block manual deletion (`409`). A pending deletion is paused and cannot be resumed, re-added or used for a new transfer; retry Delete after fixing the reported storage conflict, or explicitly remove only the record to retain the files. Automatic cleanup retries within its FIFO/limit policy; manual deletion is retried explicitly.

File/ZIP downloads open inside the download root and reject symlinks, service storage, shared/aliased paths and unconfirmed layouts (`409`). Missing files return `404`; storage failures return generic `500` errors. Legacy records without matching metainfo must not be served or deleted by guessing from their name/size. Restore their original metainfo from backup before retrying. Keep a backup before manipulating legacy data. Filesystem operations are not a sandbox against a local administrator or external writer: hardlinks outside registered storage and same-size content changes are not cryptographically detected. Current-open verification and isolated payload layout are described above.

Removed nonfunctional authentication options are no longer supported: remove old authentication CLI flags (now rejected) and environment entries (no longer read) from deployment scripts. There is no built-in access control; management API and logs are accessible to anyone who can reach the service. Use a trusted network or protected reverse proxy.

SQLite uses WAL for file databases, `synchronous=FULL`, foreign keys and a 5000 ms busy timeout on every pooled connection, including replacements. Resume checkpoint/revocation transactions also verify WAL/FULL on the connection they use. In-memory databases use a single connection and SQLite's memory journal instead of WAL; they are not eligible for trusted resume persistence.

### Single Instance Per Download Root (Phase 43)

The engine acquires an OS lock on `HUB_DOWNLOAD_DIR/.tsh-instance.lock` before
opening torrent storage and retains it until all owned tasks and storage have
closed. Another instance cannot use that same root, even with a different database
or a symlink alias to the directory. The file remains after shutdown; its presence
does not mean a process is running. **Do not delete or replace it to bypass a lock.**
The OS releases the lock after process termination; an initialization failure also
releases it. Symlink/hardlink lock files are rejected. Linux/macOS use `flock`;
Windows uses a nonblocking exclusive byte-range lock. Unsupported locking fails
startup rather than admitting competing writers. Remote SMB/NFS locking and
arbitrary external file modifications are not covered by this local-filesystem
ownership guarantee.

### Fast Resume Policy And Upgrade

`HUB_FAST_RESUME_MODE=fast|verify` (CLI `--fast-resume-mode`) defaults to **fast**,
including upgrades without an explicit setting. Unknown/empty explicit values
fail startup. Change the policy by restarting, not per request.

**Fast assumes this service is the only payload writer.** Valid sealed pieces can
be restored without hashing after clean shutdown or process crash. Size, mtime,
file identity and volume evidence reject changes but do not prove unchanged
content. Same-size edits with restored timestamps may be served before corruption
is detected. After external edits, sync, moving/locating data or restoring backup,
run **Recheck**; it preserves paused intent. Choose `verify` for a hash pass on
every open. Neither mode promises protection against malicious local access or
hardware that falsely acknowledges flush; fast does not detect all bit rot.

Old `piece_completion` rows are disposable hints, **never adopted as trust**. The
first upgraded open without a valid checkpoint hashes existing data normally; periodic
(60-second dirty owners) and bounded shutdown sealing then create checkpoints.
Missing/unsupported/oversized checkpoints fall back to ordinary verification,
not deletion or repair. Storage safety blocks still take precedence. Unsealed
progress may need checking after a crash. Database checksum alone grants no
readiness. Unsupported filesystem flush/locking semantics cannot grant trust;
local filesystem tests do not establish SMB/NFS/NAS guarantees.

REST list/details and full/compact SSE include additive `resume`: configured
`mode`, open-attempt `status`/`reason`, restored checkpoint `checkpoint_generation`
(int64 decimal **string**) and `checkpoint_at_ms`, plus current disjoint
`restored_bytes` and `freshly_verified_bytes`. Rechecking moves bytes from the first
counter to the second; invalid data contributes to neither. These are cached
attachment-local observations, not durable authority; `verification.checked_bytes`
also includes failed checks. GUI explains fallback and provides the existing
Recheck action. Polling performs no new payload stat/hash/flush.

On opening rooted managed storage, missing files and ranges beyond the actual
file size are classified as **incomplete**, without queuing pointless startup
hash attempts. This applies to both modes: absence cannot be hash-verified.
Existing untrusted bytes (including sparse files) still need hashing; every
newly downloaded piece must pass its hash before it can be read or uploaded.
This startup plan is discarded after attachment, never reused to exempt later
writes or explicit Recheck. Missing previously present data, access/volume faults
and recovery journals retain their normal safety gates; no automatic repair is
introduced. Recheck still performs a fresh pass, independent of startup evidence.

Additive `verification.skipped_missing_bytes` and file-status
`skipped_missing_bytes` report ranges excluded from the initial hash plan that
have not since had a hash attempt. They are historical planning observations,
**not current file presence, verified progress or readable bytes**. They do not
increase `checked_bytes`, restored bytes or downloaded progress. A completed
verification **queue** does not mean all payload exists or is valid. Expected
startup absence is logged as one piece-count summary rather than a warning for
every missing piece.
Accounting follows whole pieces: at a shared file boundary, present bytes can
also be excluded because another contributing file is missing/short. The counter
is therefore not a measurement of the physically absent byte count.

For diagnosing bursty downloads, `GET /api/v1/health/bt` includes optional
`torrents[].storage_io` counters for the current backend attachment:

- `completion_sync_count`, `completion_sync_errors`, `completion_sync_active`,
  `completion_sync_ns`, `completion_sync_max_ns`: completed attempts, errors,
  in-flight calls, cumulative and maximum duration of piece completion Flush.
  This includes path/open/name-lock work and sync of all contributing files,
  not just the time inside the operating system's `fsync`.
- `write_lease_wait_count`, `write_lease_wait_active`, `write_lease_wait_ns`,
  `write_lease_wait_max_ns`: contended file-payload write-lock acquisitions,
  currently waiting acquisitions, cumulative and maximum wait duration.

Compare counter deltas alongside useful received bytes and speed; nanoseconds
are cumulative, concurrent waits can overlap, and counters reset on reattachment.
They do not cover every disk/SQLite/lifecycle wait or prove which resource limits
throughput. Reading them performs no disk I/O. Verified storage no longer holds
the file-wide payload lease through completion sync: neighboring writes may
proceed, then the original hash evidence is revalidated before publication.
Hashing, required sync and Close/checkpoint barriers remain enabled.

For backups, stop the service and take a consistent snapshot of database/WAL and
payload together (or use a coordinated read-only snapshot of both). Restoring a
backup is an external mutation: use `verify` on its first open and recheck before
serving. Rolling back an entire consistent snapshot is not detectable by evidence.
For downgrade, stop first, preserve database/payload backup, and use a version
that understands this schema or restore its matching pre-upgrade backup; never
delete migration rows or treat resume tables as old completion hints. Phase 42
already ignores disposable hints for readiness, but untested downgrade binaries
are not a supported migration procedure.
Durable mutation barriers are installed for managed payload writes, rechecks and
storage workflows. A failed safety write prevents the corresponding filesystem
mutation; disposable completion-hint queues are not used for these barriers.

Reproducible local-FS hashing, WAL transaction, online-seal and Linux syscall
measurements are documented in [the measurement guide](scripts/README-resume-measurement.md).
Process-kill tests do not establish power-loss durability or remote-FS guarantees.

## Saved Logs And REST Access

Application, standard Go log, HTTP access and torrent library logs go to the console and daily files such as `hub-2026-10-20.log`. Library debug tracing is not enabled. Files use the process's local timezone, matching their timestamps; Docker commonly uses UTC. Restarting appends to the existing file.

With the standard database path, files live in `/config/logs` (host directory `./config/logs` in Compose), so they survive container restarts. The service user must be able to write there. New directories use permissions `0700`, new files `0600`; existing permissions are not changed.

Retention `3` keeps today plus the previous three calendar dates: on October 20, October 17–20 are retained. Cleanup runs on startup, on rotation and hourly even without new messages. It only removes matching regular log files, excluding the active writer and files being downloaded. File errors (including a full disk) are queued to console at most once a minute; file writes are retried after a minute.

Files rotate at the per-file byte limit into numbered segments such as
`hub-2026-10-20.000000000001.log`. Reads/tail include legacy daily files followed
by segments in numeric order; restarting appends rather than overwrites. The
date is captured on enqueue, so midnight backlog retains its event date. Before
append, the total budget removes the oldest unpinned inactive managed files,
even within retention dates. Existing oversized legacy files are never enlarged
or truncated. If protected files or deletion failures prevent freeing space,
file output pauses instead of growing beyond the budget. Quota retries are at
most once per minute, or on the next write after a snapshot releases its pin.
Already oversized protected history cannot be shrunk until it is unpinned.

Production log delivery is asynchronous, with independent file/console queues:
each retains at most 512 waiting records / 2 MiB, plus one in-flight record.
Messages are masked before enqueue and truncated to 64 KiB with a visible marker.
Saturation drops new records, including warnings/errors, rather than blocking the
engine. Logs are best-effort diagnostics, not a durable audit journal. Log reads
show completed writes and do not implicitly flush queues. Graceful logging close
waits up to two seconds; an uninterruptible sink may keep its fixed worker until
the write returns, without unsafe concurrent closure or replacement workers.
`GET /api/v1/logs/status` reports independent queue, drop/severity, truncation,
storage-error/loss/quota and close-timeout statistics without opening files or
waiting for sink I/O. Completed counts mean attempted delivery, not fsync or
success: also inspect errors and storage losses. A close timeout reports pending
work, not proven data loss: the worker may still finish. Post-close writes are
counted separately as rejected. These diagnostics require the same network
protection as the logs endpoint.

```bash
# Today; optional last N physical lines
curl 'http://localhost:8080/api/v1/logs'
curl 'http://localhost:8080/api/v1/logs?tail=200'

# A single date, or an inclusive date range
curl 'http://localhost:8080/api/v1/logs?date=2026-10-19&tail=200'
curl 'http://localhost:8080/api/v1/logs?from=2026-10-18&to=2026-10-20&tail=500'
```

The response is plain UTF-8 text with `Cache-Control: no-store` and CORS `*`. Without dates it selects today. `date` cannot be combined with `from`/`to`; both range boundaries are required. Dates must strictly match `YYYY-MM-DD`. Duplicate/empty parameters and invalid combinations return `400`. `tail` accepts 1–100000 and selects the last N lines across all selected files, returned oldest first. Stack traces count as multiple lines. Fewer than N available lines returns all of them.

Missing days inside a range are skipped. No matching file returns `404`; an empty existing file returns `200` with an empty body. Other read failures return a generic JSON `500` before transfer starts; a transfer error never appends JSON to the log text.

Responses are finite size-bounded snapshots, not live-follow (`tail -f`). Bytes appended after opening are excluded; an unfinished last line is included as it was captured. When combining files, a newline is inserted between nonempty files if the preceding file lacks one, preventing lines from different dates merging. A trailing newline is not counted as an extra empty line. Reading/tail uses bounded buffers, including for very long lines.

Sensitive query parameters are masked before console/file output. Access logs omit query strings and headers. Handled HTTP panics include their stack trace, but arbitrary direct writes to stdout/stderr and fatal Go runtime dumps are **not** captured in daily files.

**Security:** there is no built-in authentication. Anyone with network access to the service can read its logs. Keep it on a trusted LAN or protect it with an authenticated reverse proxy; permissive CORS is intentional for Smart TV compatibility. Masking known parameters is not a guarantee that all log content is free of sensitive data.

## Build Locally

Requirements:

- Go `1.25.8` or compatible.
- Node.js `22.15.0` (the version used by CI/Docker).
- npm.

Install frontend dependencies:

```bash
make deps
```

Build Web GUI and Go service:

```bash
make build
```

Run locally with default LAN-oriented settings:

```bash
make run-local
```

Run both Go modules, frontend tests and frontend build:

```bash
make test
```

`make test-root`, `make test-fork` and `make test-web` run individual suites.
`make race` covers the root module and fork core/storage; `make vet` checks both.
`make check` combines tests, race, vet and the application build. Run `make deps`
first on a fresh checkout. The fork package list is explicit (`.` and `./storage`)
and must grow if changes touch other fork packages.

The Checks workflow runs Linux integration/race and native macOS/Windows storage
suites on PRs. Release binary/image jobs depend on the same workflow. Cross-build
alone does not establish runtime filesystem support.

## Releases

GitHub Actions builds releases when a version tag is pushed:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The release workflow publishes:

- macOS `arm64` binary archive.
- macOS `amd64` binary archive.
- Windows `amd64` binary archive.
- Linux `amd64` binary archive.
- Docker image to `ghcr.io/ivangolenkov/torrent-stream-hub`.

The Docker image receives the version tag and `latest` for tagged releases.
