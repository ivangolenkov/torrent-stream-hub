# State events and effects (phase 39)

## Runtime authority

`runtime_policy.go` is the production pure policy. Its inputs are independent
applied run intent, slot ownership, operation/checking/uncertainty/initialization,
disk latch, separately owned metadata/selection/missing/legacy/verification faults,
and selected/whole-payload completion. Phase/error are outputs. Display Paused has
precedence over disk, then missing/metadata/selection/legacy/verification faults,
then current-open whole completion (Seeding), slot (Downloading), otherwise Queued.
Checking/initialization/terminal veto active projections and payload admission.

Registration/full activation alone accept validated compatibility phase imports;
historical Downloading/Seeding/Streaming never import a slot or readiness. Run
changes only through registration, explicit activation or Pause; staging does not
fabricate committed intent. Verification changes only its own fault/operation
axes, never another fault or disk key. Selected completion releases the slot;
whole completion alone may project Seeding. Runtime gates, base priorities,
metadata admission and stream overlays consume independent policy facts, not
`mt.state`. That field is a diagnostic/transport cache.

The former phase-based policies live ONLY in `*_reference_test.go`; they are
frozen regression oracles, not alternate production entry points. A sequence test
compares preserved historical behavior; independent-axis matrix and integration
tests cover the new authority and combinations the old model could not represent.

Slot grants issue a monotonically changing attachment-local token. Payload
preparation reserves a pointer ticket carrying policy epoch, runtime fence and
slot token, copies probe/metainfo inputs, releases admissionMu for probe/cache I/O,
then validates before priority/Allow effects. Concurrent preparations on one
runtime conflict, not spawn retry workers. Failed/stale preparation retires only
its own ticket; scheduler cleanup cannot release a replacement slot token.
Unchanged selection observation does not advance policy epoch; priority changes do.
Shutdown closes work admission, quiesces, and joins owned preparation without
holding admissionMu. Registration/Drop reserve their namespace through I/O;
Resume/restore/metadata/absence work uses generation/epoch/binding validation.

Registration accepts only detached phase projections (Paused/Error/MissingFiles/
DiskFull/Queued); historical Downloading/Seeding/Streaming normalize to Queued.
Full restore completion rejects unknown phases or verification veto before I/O;
it removes initialization only after metadata/selection preparation succeeds.
Resume eligibility never grants a slot/readiness. Disk recovery is a capability
on the activation event, issued only after probe, write-version check and latch
persistence. Ordinary restore cannot clear the latch. Delete and Close terminalize
the runtime, revoke acknowledgment and both gates; subsequent events conflict.
Terminalization is runtime-only, not a durable paused intent or workflow removal.

`runtimeTransition` dispatches verification, selection completion/invalidation,
slot grant/release, disk block, Pause/full-Pause, staging, apply/metadata/selection/
missing faults, registration, activation and terminal Delete/Close through one
executor. The arbitrary setState helper is gone. The former phase-based functions
are test-only reference models, never production authorities.
Policy decisions perform no I/O; the executor validates runtime and attempt tokens
under admission serialization before persistence or library effects.

| Event | Preconditions / facts changed | Allowed effects | Persistence owner / failure / stale result |
| --- | --- | --- | --- |
| Register / staged Add | Canonical identity reserved; initializing, no payload admission | Register closed gates; no scheduler grant before commit | Application create transaction; failure never publishes successful Add |
| Restore / full apply | Current record/revision; desired run and full base selection | Replace selection/FIFO; project Paused or eligible Queued, never restore readiness | Application intent + workflow/block readers; failure closes gates and exposes pending |
| Pause | Current identity; durable paused intent before acknowledgment | Quiesce, cancel payload, clear effective ranges, release slot; preserve all safety keys | Application CAS; failed commit is not durable Pause |
| Resume | No workflow/verification veto; explicit retry permission | Stage, commit enabled, probe/check write generation, remove only authorized blocks, enter scheduler | Intent CAS and dedicated disk persistence; fail closed on every partial failure |
| Priority / metadata / policy | Validated command and current revision | Apply only its own axis; cannot acknowledge previous failed apply or clear blockers | Narrow intent writers; no runtime-to-intent snapshot writes |
| Metadata ready | Current runtime pointer; valid matching catalog | Persist metainfo, ingest catalog, reconcile priorities; no implicit Resume | Catalog writer; metainfo failure vetoes initialization; stale pointer ignored |
| Scheduler grant / release | Enabled, no veto, incomplete selection, reservation available | Reserve slot, unlock/prepare, validate token, apply priorities and final payload gate; release only owned token | Runtime scheduler; never commits run intent; FIFO/capacity rechecked at grant |
| Selection changed / payload invalidated | Verified current-open pieces; current generation | Full payload -> Seeding; selected-only complete -> Queued without slot; never certify persisted bytes | Runtime observation; no intent or safety-key changes |
| Disk probe / payload write failure | Current runtime/write-error generation | Sticky latch, cancel payload, close gate, release slot; Paused display takes precedence | Dedicated disk writer; only explicit Resume after checks/persistence clears latch |
| Verification begin | Current runtime, metadata, no readers/other verification/initialization | checking + uncertainty; invalidate selected completion; release background slot immediately; close payload/upload and priorities | Persist verification block before verifier; failed persistence/task admission leaves uncertainty and gates closed |
| Verification succeeded | Same runtime and active attempt; verifier succeeded AND removal persisted | Clear only verification uncertainty/error; preserve Pause/disk/other error; recompute from current facts, not a saved pre-check phase | Verification block writer; persistence failure becomes failed verification; scheduler decides subsequent grant/readiness |
| Verification failed / cancelled | Same runtime and active attempt | End operation, retain uncertainty, close payload/upload; dedicated verification error, not InvalidTorrent; preserve other safety causes | Verification block remains; explicit successful local retry required, Resume cannot bypass |
| Restore verification block | Current bound runtime; durable verification key present | Install uncertainty and close payload/upload; release slot; do not invent operation history | Application restore; does not clear any durable key |
| Apply failure | Captured pointer, record and nonobsolete revision | Initialization veto, applied zero, closed gates, release background slot | Bounded runtime diagnostic; only full explicit Pause/Resume or reopen reconciles |
| Missing expected payload/root | Read-time absence of final and part names after current-open verified proof; not ordinary absent pieces/metainfo/hash mismatch | MissingFiles safety block, initialization veto and closed gates; Paused/disk/foreign display preserved | Current-attachment safety writer; only authorized retry allows network repair, not byte readiness |
| Delete / migration | Durable workflow reservation before destructive work | Close admission/sessions, cancel old generation; reserve/unlock/work/validate/finalize | Durable journals, command/root handles and work/namespace tickets; late callbacks cannot touch re-add |
| Close | Stop task admission and cancel generations | Close gates, join owned tasks outside locks needed by tasks | Runtime lifecycle; no new intent; late callbacks do not reopen anything |

## Durable restore projection (implemented)

`usecase.restoreActivation` projects paused intent before disk, then MissingFiles,
metadata-persistence/invalid-selection, legacy/unknown activation block,
verification, otherwise Queued. DB-only REST/SSE
use the same projection, not a historical operational snapshot. Typed keys decide
ownership: arbitrary diagnostic reason text (even "Verification failed" on a
legacy key) does not make an error verification-owned. Full reason text remains
in `control.activation_blocks`; generic legacy display is Activation blocked.
Pause/disk dominance is display-only: no key is removed by this projection.

Every restore with activation keys uses explicit inactive networking admission,
including DiskFull combined with verification. Successful local verification may
recompute readiness and phase from current facts, but cannot enable networking
or remove any unrelated key.

## Scheduler/selection integration contract

Pause, full-Pause reconciliation, restore staging and fail-closed faults are
integrated events. Pause is allowed during verification but preserves its axes;
only a full committed selection reconciliation may remove initialization veto.
Staging conflicts with any active/uncertain verifier before changing priorities,
closes both transfer directions and releases the background slot. It acknowledges
no revision; applied becomes zero until full apply/bind. Apply failures likewise
invalidate acknowledgment, preserve Paused/safety phases and require explicit retry.
Metadata-persistence and invalid-selection faults close both gates, invalidate
applied acknowledgment and set initialization veto. Display preserves Pause/disk/
foreign safety errors. Dedicated producers add typed durable keys after closing
gates; initial binding carries pre-binding faults before acknowledgment. Command
failure attaches/persists newly committed records before returning, not on a later
sync tick. Metadata cache directories/symlinks are not accepted as regular files.
`quiesce.go` executes the accepted close/cancel/clear-priorities effects under
admissionMu, after releasing ManagedTorrent.mu. Even metadata-pending Pause closes
upload. A failed selection is Invalid file selection, not invalid metainfo.
Pause may quiesce before its intent transaction; applied zero at this point is not
a promise that durable paused intent committed. Only successful full apply/bind
acknowledges the saved command. Staging cannot replace a verification veto with
initialization or use it to clear the durable verification key.

Physical missing-payload detection uses only read checks plus previously observed
current-open verification. That expectation survives asynchronous storage readiness
invalidation, is bounded by the file catalog, and resets on explicit repair. Lstat
does not follow a final symlink. Runtime storage uses metadata names; legacy
`.part` import finishes before attachment, and a later `.part` neighbor cannot
mask disappearance of the expected metadata path. No
periodic scan is added. Both directions close before safety persistence; persistence
failure remains an error, not a claim that the block survived a crash.

Runtime fault insertion may follow an earlier command revision on the SAME current
record/attachment: a commit/apply gap must not discard physical safety evidence.
It is add-only and limited to metadata/selection/missing keys. Replaced attachment,
future revision or re-add rejects the write. Verifier removal remains exact-fenced.
Only explicit Resume atomically removes retryable keys with its enabled intent;
verification/workflow/disk recovery retain their separate authorities.


Selection observations carry current-open verified completion for both the whole
payload and base selection, plus metadata-known. Unknown metadata and inconsistent
proof (whole complete but selection incomplete) reject without effects. A checking,
uncertain or initializing runtime does not accept completion observations. Pause,
Error, MissingFiles and disk veto retain phase; selection observations clear none
of these axes. Only full payload permits Seeding; selected-only completion moves
eligible Downloading/Seeding to Queued and releases base priorities/background gate
without cancelling independent stream overlays.

A slot request requires enabled run, no existing slot, incomplete selection, no
initialization/verification/fault/disk veto and current available capacity/FIFO.
The displayed Queued string is not authority. Preparation releases admissionMu;
the reserved slot counts against capacity, and its token must still be current
before effects or cleanup. A new reservation cannot be released by an older
attempt. Error/DiskFull/Paused axes are never overwritten by unconditional Queued
cleanup. Stale runtime events conflict before mutation.

Disk-block installation uses the same policy and preserves verification/activity
axes and Paused display. The existing effect path closes payload/cancels work
before dedicated latch persistence; stale runtime and empty-reason events reject
without persistence or library effects. Clearing the latch is still exclusively
the explicit Resume probe/write-generation/persistence path, not a policy shortcut.

## Verification projection and authority

Paused display dominates the disk latch, but does not erase it. A disk latch then
dominates verification's display error. Other Error/MissingFiles causes are not
owned by verification and are preserved. Only the dedicated `Verification failed`
error may be cleared by a successful verification. Failed/cancelled checks and
failed block persistence keep upload and payload closed independently of display.
NoPeers/tracker diagnostics are not evidence of invalid metainfo or missing files.

Completion must carry its attempt token: a duplicate completion from a previous
check cannot finalize a later check on the same runtime. Delete/re-add additionally
invalidates the runtime pointer. Intent changes during a check (notably Pause) are
read at completion; a check never restores its entry state. Streaming remains an
independent session activity; verification admits no payload reader concurrently.
