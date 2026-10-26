# Фаза 43 — протокол fast resume, версия 1

Статус: **фаза 43 завершена; блоки 1–6 и platform gates закрыты**. Producer/scanner/finalizer установлены автоматически; guarded writable restore подключён при backend open. Public constructor читает restart-scoped config с default fast; REST/SSE/GUI различают restore и hashing. Расширенная crash/fault matrix и transaction/fsync/online-seal measurements завершены. Успешный CI, закрывающий ожидавшийся native Windows gate, подтверждён пользователем сообщением «CI прошел». Основание: [план](phase43-trusted-fast-resume.md) и [отчёт](implementation/phase43-report.md).

## Политика и граница доказательства

- Настройка `HUB_FAST_RESUME_MODE=fast|verify` / `--fast-resume-mode`; отсутствие настройки означает `fast`, неизвестное или явно пустое значение — ошибка конфигурации, не best-effort trust. Изменение требует restart. Zero-value Go Config сохраняет default fast.
- `fast`: пригодный committed checkpoint минус durable revocations, отфильтрованный по текущему evidence. Clean и dirty session проходят одинаковую валидацию. Только факт crash не отзывает checkpoint.
- `verify`: checkpoint не импортируется; открытие использует действующий hash-проход. Переключение требует restart; explicit recheck отзывает соответствующее доверие до запуска hashing.
- Предположение: payload обычно изменяется сервисом. Это default policy, а не подтверждённая исключительность внешнего доступа. Same-size правка с восстановленным mtime, bit rot и клонирование носителя могут остаться незамеченными до явного recheck. File identity/mtime/size/marker — отрицательные сигналы при несовпадении, не hash-доказательство при совпадении.
- Межпроцессный OS lock на управляемый download root обязателен до storage admission; он не исключает SMB/редактор/restore backup. На неподдерживаемой FS/lock/flush semantics fast restore отключается с диагностикой, но две конкурирующие instance не получают writable admission.
- Не обещать автоматический scrub, filesystem watcher, обнаружение всех внешних правок, восстановление новых pieces после последнего checkpoint либо абсолютную защиту от power loss. Подтверждённый FS flush и SQLite commit дают только документированные гарантии конкретной среды.

## Разделение hints и authority

Существующий `piece_completion` остаётся disposable cache с LRU 4096/pending 1024; его `Set`, async flush, пропуски очереди и `Persistent()` не создают checkpoint. Ни старые строки, ни inferred completion по размеру/имени не импортируются как trusted pieces.

`internal/fastresume` блока 1 — pure policy: `Checkpoint.Validate`, `Filter`, budgets и причины. `Candidate` не является restore token, не выполняет rooted opens и не разрешает transfer. Его можно получить только после собственного сбора evidence; дальнейший storage adapter обязан повторить owner/generation/attachment fencing перед импортом.

## Identity и представление

Checkpoint одного владельца включает:

| Поле | Смысл |
| --- | --- |
| schema | 1; неизвестная версия даёт fallback |
| record_id, storage_id | Защита delete/re-add и владелец aliases |
| mapping_revision, layout_version, root, prefix | Точная сохранённая раскладка, не вычисление из title/hash |
| metainfo_digest | SHA-256 точных info bytes canonical owner, не payload |
| catalog_digest | SHA-256 канонического ordered catalog/geometry и необходимых проверенных v2 layers; формат реализован в `internal/fastresume/storage.go`, проверен conversion/integration tests |
| volume_id | Persisted enrollment evidence; live device ID не сохраняется как межсессионная identity |
| piece_length, piece_count, extent_length | Geometry fork; extent включает v2 alignment, пользовательские file lengths — нет |
| generation, created_at_ms | Положительная int64 generation snapshot и время seal |
| files | Ordered metainfo catalog: root-relative physical path, offset, logical length; evidence present/native file identity/actual size/mtime ns |
| pieces | LSB-first bitmap verified/trusted subset; точная длина ceil(piece_count/8), неиспользуемые tail bits равны 0 |

File catalog сверяется с подтверждённым metainfo и resolver, а не принимается как инструкция открытия произвольного файла. Границы file extents упорядочены, без перекрытий, с допустимыми alignment gaps; paths проходят существующие containment/collision правила. Unknown/absent file evidence не даёт trust пересекающим pieces. Zero-length не получает readiness из отсутствия pieces — требуется реальный rooted open.

Hybrid aliases разделяют одного canonical owner и один checkpoint; произвольный alias не может подменить canonical digest. Если невозможно доказать соответствие v1/v2 geometry и layers, fallback, а не перевод bitmap по строковому hash. Padding не считается пользовательскими verified bytes.

Канонизация catalog: domain `TSH-file-catalog-v1`, raw SHA-256 точных info bytes,
little-endian int64 piece length/count/extent/file count; затем ordered files
(length-prefixed physical relative path, offset, length), int64 required layer count
и отсортированные по root пары root/layer. Здесь string length — int64, в отличие
от uint32 envelope codec. Привязка включает только необходимые валидированные v2
layers; посторонние entries не создают trust. `FromStorage` требует точного
совпадения native catalog/geometry/hash/root; `StorageClaims` повторяет binding
и вычитает durable revocations без изменения сохранённого bitmap. Info должен
приходить из canonical validated attachment, не из произвольного HTTP объекта.

## Durable хранилище (блок 2)

Additive migration выделяет `resume_owners` и `resume_checkpoints`, независимо от `piece_completion`:

- `resume_owners`: storage ID, record ID, mapping revision, монотонный mutation epoch, текущая checkpoint generation, invalidated flag. Tombstone/record fencing не позволяет старому worker воскресить удалённого владельца.
- `resume_checkpoints`: одна committed generation владельца, schema, identity/catalog/file evidence, bitmap, durable revocations bitmap, created time и integrity digest serialized metadata/bitmaps. Digest обнаруживает случайную порчу snapshot, не является защитой от злонамеренной правки БД.
- Поля identity и CAS сверяются с authoritative mapping/record в той же короткой транзакции. В `Seal` worker передаёт захваченные record/mapping/generation/mutation epoch. Commit неудачен, если epoch изменился: старый snapshot не очищает поздний отзыв.
- Смена generation, публикация payload checkpoint и сброс учтённых revocations — одна SQL-транзакция. До commit видна предыдущая generation с её отзывами, после — только новая согласованная. Незавершённый seal не является второй authority.
- Таблицы/поля окончательно закреплены этим контрактом по смыслу; при необходимости детализации SQL ограничения отражаются в отчёте до использования runtime. Durable writes не используют lossy hint queue.
- Safety connection требует `journal_mode=WAL`, `synchronous=FULL` и настроенного busy timeout. Настройка и проверка делаются для каждого используемого pooled/replacement connection; default драйвера не считается доказательством. SQLite DB закрывается после join storage/seal tasks. Не менять режим PRAGMA внутри активной транзакции.

Реализация: migration `0020_resume_checkpoints.sql`, `internal/repository/resume.go`.
`CaptureResume` создаёт/перепривязывает invalidated head и выдаёт CAS token;
`LoadResume` читает без отзыва; `CommitResume` публикует следующую generation;
`RevokeResume` синхронно добавляет bits отзыва, `InvalidateResume` отзывает владельца.
CAS token не является runtime read/write permission. Ошибка commit возвращает
ошибку, даже если исход commit неопределён: вызывающий код не допускает write,
пока reread не установит durable state. Повтор уже подтверждённого отзыва не
увеличивает epoch и не переписывает bitmap.

Integrity SHA-256 связывает storage/record IDs, mapping revision, epoch, generation,
длины и точные bytes envelope/revocations. SQL CASE ограничивает возвращаемые blob
**до** выделения driver/Go result; binary decoder ограничивает counts/strings до
`make` и проверяет tail bits/отсутствие trailing data. Один payload envelope ≤4 MiB;
отдельная revoked mask ≤512 KiB. Два глобальных working slots берутся до SQL,
не под занятым writer; ожидание отменяемо. Decode read snapshot происходит после
закрытия read transaction. Файлового I/O нет ни в одном из этих методов.

`internal/fastresume/codec.go` закрепляет бинарный формат: magic `TSHRSM01`, затем
little-endian int64 schema/generation/time; семь length-prefixed strings
record/storage/root/prefix/metainfo digest/catalog digest/volume; int64
mapping revision/layout/piece length/piece count/extent/file count; для каждого
файла string path, int64 offset/length/present(0|1), string native identity,
int64 size/mtime; в конце raw LSB bitmap. String prefix — uint32 bytes. Native
paths не становятся межплатформенной authority при копировании БД. Golden fixture
442 bytes/SHA-256 закреплена отдельным тестом; изменение формата требует решения
о версии, а не молчаливого изменения decoder.

Private `tsh-sqlite` driver hook задаёт FULL **после** сортируемых modernc DSN
pragmas на каждом новом соединении. Поэтому пользовательский DSN с OFF не ослабляет
barrier; ручная порча PRAGMA уже открытого pooled connection выявляется самой
safety operation. In-memory DB не проходит WAL gate. Остальные пользователи
стандартного драйвера `sqlite` этим hook не затрагиваются.

Engine удерживает `payload.InstanceLock` до join/закрытия storage. Lock file остаётся
на месте; symlink/hardlink или смена identity во время acquisition отклоняются.
Linux/macOS используют nonblocking `flock`, Windows — `LockFileEx` на первом byte.
Unsupported lock не допускает startup. Произвольный внешний unlink и распределённое
SMB/NFS locking не считаются защищённой моделью; локальные tests это не доказывают.

### Revoke → write

1. Существующий mutation owner закрывает конкурирующий seal/attach затронутого storage, без ожидания I/O под global/library lock.
2. До перезаписи доверенного диапазона durable transaction отзывает все пересекаемые pieces текущей generation, увеличивает mutation epoch. Truncate/replace/import могут требовать отзыва целого файла; mapping/move/locate/delete — владельца.
3. После подтверждённого commit снимается live trust и разрешается соответствующий syscall. Readiness transition синхронизирован с readers/upload: не публиковать новый read lease отозванного piece. Учесть существующие leases до destructive mutation.
4. Commit error/неопределённый результат не разрешает write. Повтор/перечитывание БД может установить факт отзыва; иначе операция останавливается. Не маскировать ошибку под успешный flush hint.
5. Запись в никогда не доверенные pieces не отзывает независимый bitmap. Уже durable-revoked bits не требуют новой SQL записи на каждый chunk. Coalescing допускается только до admission; pending revocations не теряются при saturation.
6. После crash используется last committed generation с durable revocations. Изменившийся mtime всего частично записанного файла может дополнительно отозвать его subset; correctness важнее обещания минимального числа rehash pieces.

### Quiesce → flush → seal

1. Один worker выбирает изменившегося владельца; periodic interval 60 s, также запрос при orderly close. Snapshot не запускается из `Completion.Get/Set` под library lock.
2. Приостановить mutation/hash admission **этого** владельца, дождаться активных writes, hash/marking, layer updates, imports и opens. Online seal не Drop и не disconnect read-only streams. При превышении 2 s ожидания quiesce отложить periodic seal; уже выданный syscall остаётся у владельца до завершения.
3. Захватить согласованный valid subset, исключив dirty/pending/failed pieces. Flush всех файлов, от которых он зависит, и требуемого namespace. V1 boundary piece зависит от всех contributing files. Промежуточные незавершённые writes не попадают в snapshot.
4. Удерживая per-owner mutation fence, выполнить короткий CAS commit новой generation в SQLite. При несовпадении epoch/record/mapping discard snapshot, не очищать revocations. После завершения возобновить mutation admission.
5. Failed/cancelled flush/seal не публикует новую generation. Предыдущая неотозванная generation остаётся только кандидатом, проходит evidence validation заново. Shutdown deadline не равен успешному checkpoint; незавершённую задачу нельзя считать joined.

Реализованные primitives: `Torrent.WithFileCheckpoint` использует один global
capture slot, per-torrent admission и joins hashes/layers/network writes; callback
выполняется вне `Client.lock`. Backend `TorrentImpl.WithCheckpoint` удерживает
exclusive lifecycle fence через evidence/flush/повторную проверку evidence/publish.
Drop/Close ждут callback; отмена не освобождает lock за зависший syscall. Чтение
сохраняет доступ во время publish. Installed periodic producer использует эти
primitives только с application mutation ownership и durable write hooks.

`internal/fastresume.SealWorker` реализует scheduling отдельно от authority:
одна goroutine на worker, periodic tick 60 s, не более 64 владельцев **включая
активного**, FIFO и coalescing. Изменение во время seal сохраняет следующий запрос;
ошибка оставляет владельца на следующую попытку, без busy retry в том же batch.
`MarkDirty=false` означает отказ admission (saturation/stopping/invalid owner),
а не успешное сохранение. Producer обязан хранить dirty state и повторять enqueue
при bounded inventory scan; durable revocations никогда не идут через эту очередь.

Первый `Shutdown(ctx)` останавливает admission и делает один drain pass с независимым
shutdown context. Следующие вызовы могут ограничить только собственное ожидание,
но не заменяют context активного drain. Deadline отменяет активный callback,
не выдаёт его за joined и не запускает новые seal callbacks после исчерпания бюджета.
`Done`/`Wait` завершаются только после возврата всех выданных callbacks; до `Wait`
нельзя закрывать SQLite/storage/instance ownership. Ошибки drain возвращаются
вызывающему; ошибки periodic seal должен фиксировать adapter. Production создаёт
один worker; producers останавливаются по runtime cancellation и обрабатывают отказ
enqueue. Engine `RegisterStorageFinalizer` принимает ownership одного worker до
shutdown. `Close` после runtime cancellation/quiesce запрашивает `Shutdown` с
отдельным 10-second budget параллельно с joins обычных tasks: coordinator cleanup
может ждать reservation активного seal, поэтому stop нельзя откладывать до join.
После joins обычных tasks и Shutdown выполняется безусловный `Wait`
до client/backend close и освобождения instance lock. Rejected registration не
передаёт ownership. Callback выполняется вне engine locks. Default finalizer
установлен usecase; independent context не обходит coordinator ownership.
Отклонённый installer join-ит только свой новый worker, не existing coordinator.

Дополнение adapter: `repository.ResumeSource` содержит canonical immutable
Identity/MetaInfo/Info, attachment `Authorize` и `Capture` (обычно
`Torrent.WithFileCheckpoint`). `SealResume` берёт durable CAS до capture и повторяет
authorization перед atomic commit внутри post-flush publish. `RestoreResume`
проверяет snapshot/claims и повторяет attachment + durable generation/epoch check
в native prepare/import callbacks. Info используется без повторного decode
больших info bytes, чтобы не нарушать restore memory budget.

`applicationResumeProducer` связывает worker с существующим coordinator: lookup
повторяется после sorted record/storage/root acquisition, handles удерживаются до
возврата callback. Shutdown получает одну private `coordinatorDrain` capability
только после закрытия обычного admission и join его references; capability
работает на том же registry, normal commands она не оживляет. Worker stop и переход
ownership запускаются совместно; новые drain callbacks ждут capability без refs.
Wait присоединяет обе задачи и запрещает retirement пока reservations живы.
Concrete production source повторяет exact runtime pointer/fence/applied revision,
canonical record/mapping, inventory/volume и safety gates. Installed scanner
обходит distinct storage owners страницами ≤64, читая cached revisions без FS I/O.
Shutdown до worker stop выполняет final scan с бюджетом ≤1 s из общего 10 s,
останавливаясь при saturation; ошибка scan не отменяет уже queued drain. Его
scheduling lossy, durable revocations — нет. Dirty ack относится лишь к captured
revision; новое evidence во время publish остаётся dirty.

Явный restart-scoped `NewTorrentUseCaseWithResumeMode` включает fast/verify;
обычный constructor читает проверенный engine config, default fast. В fast
`FileResumeHooks.Restore` выполняется после
native initialization, до library publication, только пока candidate не отозван
namespace import. Tracked library closedCtx/open generation и Drop/Close join
защищают initial restore до managed registration; existing magnet runtime также
требует exact pointer/fence и live safety admission. Все durable gates повторяются.
Ошибка optimization требует durable invalidation перед ordinary verify; failed
barrier отвергает open. Callback error закрывает backend, а candidate/fallback не
создаёт missing zero files. После rooted I/O token epoch повторно проверяется под
freshMu непосредственно перед import. Отмена runtime read/probe не corruption.

`PrepareRestore` выдаёт opaque one-use `RestoreToken`, связанный с backend open,
epoch и context. `RestoreCheckpoint` повторяет rooted evidence и owner callback
перед импортом freshness. Writable backend требует attachment-scoped durable
`FileResumeHooks`; без них импорт отвергается. Library completion учитывает
необходимые v2 hashes/layers и `IgnoreUnverifiedPieceCompletion`; cached completion
сам по себе эти требования не заменяет.

## Validate → attach

1. Взять instance ownership, coherent mapping/record/volume/operation view и snapshot БД; bounded read/decode с integrity check, без создания payload.
2. Проверить schema, budgets, identity, bitmap, canonical catalog/layers и storage gates. Active journal или неверный volume не позволяют обход recovery даже при совпавших размерах.
3. Rooted opens/stat каждого ожидаемого файла порциями по 128; без hash, mkdir/import/zero-file creation. Сравнить actual identity, size и mtime. Если evidence недоступно, нет fast trust для зависимых pieces.
4. `Filter` вычитает revocations и file-dependent pieces. V1 границы затрагивают соседние файлы; v2 alignment не создаёт ложной зависимости. Независимый subset можно сохранить.
5. Повторно сверить owner/record/mapping/generation/mutation epoch/attachment до импорта. Storage-issued token ограничен этим открытием; stale token после Drop/re-add/revoke не действует. Restore не аннулирует checkpoint только из-за факта открытия.
6. Fork импортирует только допустимые complete pieces с usable file signatures и необходимыми v2 layers. Остальное — обычная hash очередь фазы 42. Ни `VerifyOnOpen=false`, ни общий `Complete=true` из hints не заменяют token.
7. Реальный Range/direct/archive/upload повторяет существующий storage gate. Обнаруженная короткая read/error/signature mismatch снимает trust затронутого диапазона и планирует проверку, не automatic repair. Cached REST наблюдение не является lease.

## Budgets

| Ресурс | Предел первой версии |
| --- | --- |
| Seal/restore worker | 1; не goroutine на каждый torrent |
| Pending владельцы | 64, coalescing; revocation barrier не lossy |
| Files на checkpoint | 4096; больше — обычная проверка |
| Pieces на checkpoint | 4 194 304; bitmap 512 KiB |
| Serialized checkpoint | 4 MiB, включая metadata/bitmaps/envelope |
| Сумма file path bytes | 1 MiB; путь ≤4096 bytes / 128 components; identity string ≤256 bytes |
| Metadata/file batch | 128 entries |
| I/O scratch | ≤128 KiB; payload не буферизуется целиком |
| Дополнительная working memory | ≤32 MiB на owner, ≤64 MiB глобально; не включает существующие fork piece structures |
| Periodic interval / quiesce admission wait | 60 s / 2 s; не deadline завершения syscall |

Limits применяются **до** больших allocations при чтении БД/decoder; одного post-decode `Validate` недостаточно. Oversized/unknown snapshot даёт fallback, не усечение. Фактический memory budget import в fork проверяется отдельно: нельзя дублировать существующие per-piece map/slices без учёта стоимости.

Writable import budget: `trustedPieces*256 + files*2048 + bitmapBytes*8 <= 32 MiB`.
Восемь masks учитывают claims/proof, factory/backend pending и revoke scratch;
старый read-only коэффициент 2 больше не применяется. Untrusted chunks не
выделяют revoke mask и не выполняют safety SQL.

Production source/open дополнительно резервируют metadata decoding/materialization:
`allPieces*256 + files*2048 + bitmapBytes*8 + metadataBytes*4 <= 32 MiB`,
metadata ≤4 MiB. Этот envelope намеренно консервативнее native partial budget;
oversize означает skip seal/ordinary verify, не усечение claims. Layer size/file
count проверяются до allocations; cached decoder ограничивает file и string size.

## Реализованные mutation barriers (блок 4)

- `FileResumeHooks` создаётся один раз на backend open, до namespace effects,
  привязывается к текущему record/storage/mapping. Native pending bitmap не
  readiness; успешный seal/restore обновляет его. До publish сохраняется union
  старых/возможно committed bits, чтобы неопределённый commit не разрешал write.
- Revoke подтверждается до overwrite и потери live freshness. Known-invalid
  hash/read evidence закрывает live gate даже при SQL failure. Already discarded
  generation очищает pending cache; повторные untrusted writes не пишут SQL.
- Per-file syscall leases (общие для read/hash, exclusive для write) берутся в
  offset order. `ReadAt` проверяет freshness после lease. Missing/truncated/replaced
  file отзывает все contributing pieces до `O_CREATE`/extension/implicit zeros.
  Это не вторая application mutation lock система: namespace commands сохраняют
  прежние command/path reservations и Drop/transfer joins.
- v2 known-hash replacement выполняет durable revoke вне `Client.lock`, считается
  layer work и повторно проверяет backend/hash/closed state до применения. Ошибка
  не применяет hash и не оставляет known-invalid live readiness.
- Close атомарно закрывает read admission, ждёт file leases включая evidence SQL,
  очищает freshness и только затем закрывает handles. Старый backend не читает
  payload нового владельца при root reuse. Readers не берут exclusive seal gate.
- Rooted zero-length readiness имеет отдельный cached proof safe open. Реальная
  readiness refresh выполняется вне Client.lock, polling не делает I/O. Restore
  candidate не создаёт missing zero file; readonly и writable cases покрыты tests.
- `InvalidateResumeMutation` допускает active journal/deletion intent для untrust,
  но проверяет exact current record/mapping и FULL/WAL. Epoch увеличивается даже
  у invalidated head для fencing pending seal. Migration 0021 дополнительно
  атомарно отзывает owner при active recovery journal и mapping workflow transition.
- Main usecase устанавливает hooks в verify mode: old checkpoint отзывается до
  verification, но checkpoint admission с active journal не запрашивается.
  Initial registration без intent допускается лишь без owner/live checkpoint;
  provisional callback затем связывается с одним record, без обхода Drop joins.
- Recheck, repair, reconcile, legacy apply, migration, move/locate и manual/FIFO
  deletion используют barriers до эффекта; consistent aliases имеют одного owner,
  split layout отвергается. Selection и Pause/Resume без repair trust не отзывают.
- Evidence-driven revoke при writable prepare может увеличить epoch. Adapter
  повторяет coherent load/prepare/import не более одного раза, чтобы сохранить
  независимые valid files; failed durability operation не игнорируется.
- При shutdown только checkpoint flush получает независимый job context. Runtime
  writers остаются cancelled; deadline не освобождает lease за зависший syscall.

## Точки интеграции, найденные в блоке 1

| Область | Текущие точки и требуемая граница |
| --- | --- |
| Engine open | `internal/engine/client_config.go:newTorrentClient`, `storage_roots.go`: сейчас VerifyOnOpen=true, import legacy parts; restore должен предшествовать любому import/create, не просто отключить VerifyOnOpen |
| Normal writes | fork `storage/file-torrent-io.go:WriteAt`, `file-torrent.go:openForWrite/acceptFileWrite`, `file-io-root.go:open/openForWrite`: отзыв до open с create и до первого эффекта; acceptFileWrite после write слишком поздний barrier |
| Mark/hash | fork `storage/file-piece.go:BeginVerification/MarkComplete/MarkNotComplete`, `torrent.go:hashPiece/VerifyDataContext`, layer update под storageLock: seal snapshot после join; MarkNotComplete и failed hash отзывают persisted trust |
| Namespace | fork `storage/file-client.go:OpenTorrent` (zero-length create), `file-import-part.go`, `file-piece.go:promotePartFile/onFileNotComplete`, `file-io-root.go:rename`: topology mutation требует отзыва до эффекта |
| Read gate | fork `storage/file-torrent.go:freshCompletion/refreshRange/cachedCompletion`, `torrent.go`, `reader.go`, `file.go`; engine `stream_source.go` direct/archive/stream и `missing_payload.go`: единый validated trust и отзыв после ошибок |
| Offline verify | `internal/engine/verify_storage.go`: candidate analysis остаётся read-only и не импортирует чужой checkpoint; explicit runtime recheck отзывает свой |
| Move/copy/locate | `internal/usecase/storage_commands.go`, `storage_worker.go`, `storage_move.go`, `storage_move_copy.go`, `storage_locate.go`: coordinator ticket → revoke owner → journal/FS; copy fingerprint не completion |
| Legacy/repair | `internal/usecase/storage_migration.go`, `legacy_recovery.go`: source/target/quarantine/import защищаются existing ownership + revoke до эффекта |
| Delete/temporary | `internal/usecase/deletion*.go`, `temporary_files.go`, engine Drop/Close: join old jobs, tombstone checkpoint до unlink, cleanup не воскрешает trust |
| Volume/inventory | `internal/payload/volume.go`, usecase `storage_inventory.go`, enrollment/reconcile: marker не hash evidence; unexpected volume не создаётся при restore |
| Projection | `internal/usecase/file_projection.go`, engine `health.go`, fork `file-observations.go`, models/REST/SSE/Vue: restore bytes отдельно от hash-attempt counters; никакого I/O на polling |

Этот реестр сохранён как исходный integration checklist блоков 3–5; producer, hooks и configuration/default/projection реализованы. Помимо production file backend остальные backend не получают fast trust автоматически. Изменения `mmap` backend не требуются и mmap для payload не включается.

## Additive read model (блок 5)

Torrent получает необязательный `resume`: `mode` (`fast|verify`), `status` (`pending|restored|partial|fallback|disabled`), `reason`, `checkpoint_generation` (десятичная строка int64), `checkpoint_at_ms`, `restored_bytes`, `freshly_verified_bytes`. Generation не заменяет record/mapping/runtime_attachment fencing.

Причины pure filter: `restored`, `strict_mode`, `no_checkpoint`, `invalid_checkpoint`, `budget_exceeded`, `identity_changed`, `storage_blocked`, `files_changed`, `pieces_revoked`. Адаптер добавляет типизированные `checkpoint_io_error`, `checkpoint_integrity_error`, `unsupported_storage`; текст OS/SQL errors не API-коды. `files_changed/pieces_revoked` могут сохранять независимый subset, это не всегда full fallback.

`restored_bytes` — текущие still-valid bytes из checkpoint, `freshly_verified_bytes` — текущие bytes, подтверждённые hashing этого attachment; пересечение не суммируется дважды. После нового hashing piece переходит из restored в freshly verified. CheckedBytes продолжает считать реальные завершённые hash attempts, включая неуспешные; restore не увеличивает его и не создаёт фиктивную verification operation.

Status/reason и generation/time относятся к попытке restore **этого backend open**,
а не к последнему periodic seal. Они заполняются до publication и затем immutable.
Counters — sampled current readiness; safety/recheck gates могут обнулить их.
Источник помечен в существующих native per-piece evidence, без новой bitmap или
копии inventory. File extents исключают v2 alignment gaps и двойной учёт v1
boundaries. Detached/stale attachment теряет runtime diagnosis, получает pending
storage_blocked с нулевыми bytes и configured mode. Причина partial `restored`
допустима, если checkpoint изначально содержал subset и evidence не менялась.

Legacy progress/selection остаются долей usable complete pieces с документированным источником доверия для fast mode. GUI показывает «Восстановлено из сохранённого состояния», а не «Проверено в этой сессии». Компактный SSE не содержит catalog/bitmap. Wire fixtures Go/TS и invalidation details проверяются при подключении DTO, до выпуска default fast.

## Приёмка

Блоки 1–2 закрывают policy/durable store/OS lock, блоки 3–4 — installed producer/restore и mutation barriers. Настройка/default/projection блока 5 и расширенные crash/fault tests/measurements блока 6 выполнены. Final macOS root/fork/race/frontend checks и native Linux suites прошли локально. Ожидавшийся native Windows platform gate закрыт на основании последующего пользовательского подтверждения успешного CI; cross-compilation/старое подтверждение фазы 42 не использовались вместо этого результата. Process kill по-прежнему не доказывает power-loss durability, local FS — NAS/SMB/NFS semantics.

Финальные crash probes наблюдают existing seal ownership через optional IOEvent
snapshot/payload-sync/directory-sync before/after, не создавая новый admission
mechanism. Reopen проверяет exact last committed generation/subset, не только
успех metadata decode. Transaction measurements считают WAL write commit frames;
helper return, SQL rows, write transactions и actual OS fsync calls — разные
величины. Actual Linux strace и reproducible parser/instructions находятся в
`scripts/README-resume-measurement.md`; power-loss не заявляется испытанным.
