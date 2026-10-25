# Фаза 41 — контракт восстановления (блок 1)

Wire-контракт фазы 41. Первоначально зафиксирован в блоке 1; затем подключён к production workflows, REST/SSE и GUI. Текущий статус приёмки и результаты платформенных проверок приведены в [отчёте](implementation/phase41-report.md).

Совместимость с фазой 42: [контракт хранения](phase42-storage-contract.md) добавляет layout 2, storage identity/revision, move/locate claims и inventory без переписывания manifests/journals фазы 41. Старые isolated hash roots продолжают работать. Pending deletion/cancel, сохранённые alternatives и их ownership не очищаются SQL upgrade. Первичное enrollment старого тома разрешено даже для единственной blocked записи; оно сохраняет journal и не является retry/Resume. Для downgrade нужна согласованная offline-копия БД/metainfo/payload/retained data/volume markers; старый бинарник не должен запускаться на новой раскладке.

## Независимые оси состояния

- `storage_state` остаётся состоянием layout (`legacy`, `blocked`, `moving`, `ready`). `ready` не подтверждает attachment или целостность payload.
- `metadata_known` определяется сохранённым metainfo/каталогом, а не наличием runtime.
- Новый необязательный `storage_recovery` описывает причину и разрешённые действия. Отсутствие поля означает отсутствие диагностики, а не доказанную готовность.
- `storage_recovery.runtime`: `absent`, `attached`, `failed`.
- `storage_recovery.data_readiness`: `unknown`, `checking`, `verified`. Доступ разрешают существующие current-open verification gates, а не это наблюдательное поле.
- Старые `error`, `storage_error`, `deletion_error` сохраняются. Клиент не разбирает их текст для определения действия.

## Identity и версия

`storage_recovery.record_id` — identity текущего экземпляра записи, не hash. `version` — десятичная строка int64, чтобы JavaScript не терял точность. Это **версия recovery operation**, не layout version и не snapshot version. Действия последующих блоков должны принимать ожидаемые `record_id` и `version` и повторно проверять их до изменений. Старые запросы и callbacks не применимы к re-add.

Один и тот же объект используется в списке, `GET /api/v1/torrent/{hash}`, compact/full SSE и action errors. Для ошибки действия сохраняется верхнеуровневый `error`, добавляются `code` (типизированная причина из таблицы) и `storage_recovery`. Команды используют `POST /api/v1/torrent/{hash}/action` с полями `action`, `record_id`, `version` (строка); `apply_legacy` также требует `choice=final|part`. Heavy recovery и запланированный ownership retry возвращают 202; результат наблюдается через REST/SSE. Deletion retry/cancel/forget возвращают 200 после завершения.

## Причины и действия

| `reason` / error `code` | Действия (при выполнении safety gates) |
| --- | --- |
| `legacy_file_conflict` | `analyze_legacy`; после анализа — `apply_legacy` |
| `deletion_pending` | `retry_deletion`, `cancel_deletion` |
| `deletion_mode_conflict` | 409; исходный режим сохраняется; `retry_deletion`, `cancel_deletion` |
| `deletion_cancelled` | `forget_record`, `resume_verify` |
| `ownership_catalog_changed` | `retry_ownership` после ограниченного числа автоматических повторов |
| `ownership_conflict` | `retry_ownership`; не выдаётся за доказанную кратковременную смену каталога |
| `migration_queued` | ожидание; пустой список действий |
| `migration_active` | ожидание; пустой список действий |
| `attachment_failed` | `resume_verify`, если нет более приоритетного gate |
| `disk_full` | пустой список; sticky disk latch не снимается recovery |
| `recovery_complete` | `resume_verify`; защищённые альтернативы остаются |

Список `actions` — фактически доступные команды, а не универсальная таблица разрешений. Pending deletion имеет приоритет над legacy retry/обычным Resume. Ошибки I/O/SQLite нельзя автоматически переводить в 409. `metadata_path` и `progress.file_path` содержат относительное metadata-имя, не внутренний абсолютный путь. Выбор кандидата не основан на размере/имени и не разрешает readiness.

## Удаление

`deletion.mode`: `keep` или `payload`; несовпадающий запрос не меняет intent.

`deletion.stage`: `intent` → `runtime_stopped` → `manifest_ready` → `removing` → `removing_record`. Отдельная ветка: durable `cancel_requested` → ожидание владеющего worker → `cancelled`. После подтверждения отмены старый worker не имеет права выполнять unlink или удалять запись.

`deletion.removed_data`: `none` (есть доказательство отсутствия выполненных удалений), `partial` (известны выполненные удаления), `unknown` (старый manifest/неопределённая граница сбоя). Отмена не восстанавливает удалённые файлы и не запускает загрузку. `deletion.error` — пояснение последнего отказа, не код состояния.

## Прогресс

Объект `progress` привязан к identity/version. `stage`: `queued`, `preflight`, `checksum`, `copy`, `verification`, `cleanup`; для recovery также `analysis`, `preserve_alternative`, `publication`. Verification этапа migration не заменяет verification нового runtime.

Поля:

- `queue_position` — 1-based место в последовательной HDD-очереди, опционально;
- `bytes_done`, необязательный `bytes_total` — текущий этап, не сумма повторных чтений;
- `files_done`, необязательный `files_total`;
- `file_path`, `file_bytes_done`, необязательный `file_bytes_total` — текущий файл;
- `stage_started_at_ms`, `last_progress_at_ms`, `heartbeat_at_ms` — Unix milliseconds.

Отсутствующий total означает неизвестный объём, 0 — известный пустой этап. Heartbeat не обновляет `last_progress_at_ms`, не делает filesystem I/O и не подтверждает продвижение syscall. Целевая периодичность ≤5 секунд, публикация latest-only, без per-chunk SQLite journal. Смена этапа сбрасывает stage counters, поэтому общий процент/ETA из них не вычисляется.

Счётчики этапа отражают фактические чтения, включая safety rereads. Copy может читать исходник и повторно читать временную копию; totals для неоднозначного многопроходного этапа опускаются. Файловый total, если известен, учитывает эти проходы. Checksum/copy повторяются пакетами до 64 файлов с отдельными stage counters; это не единый процент всей миграции. Heartbeat формирует независимый двухсекундный sampler без worker на каждое обновление.

## Расширения подключённой реализации

- `analysis`: `path`, `equal`, необязательный `recommended=final|part`, `final`/`part` evidence (`size`, `valid_pieces`, `total_pieces`), `limitations`.
- `alternative_copies`, `alternative_bytes`: число и размер сохранённых защищённых копий; внутренние backup paths в API не раскрываются.
- `failure_code`: `candidate_changed`, `candidate_missing`, `unsafe_candidate`, `target_exists`, `operation_unsupported`, `flush_failed`, `disk_full`, `operation_busy`, `io_error`. Не подменяет workflow `reason`, поэтому retry применяет тот же journal/choice.
- `repeat_count`: число одинаковых migration failures в текущем процессе; не completion и не durable checkpoint.
- Persistent migration `0014_storage_recovery.sql` добавляет отдельную ownership revision и recovery table. Старые cleanup manifests импортируются с исходным mode и `removed_data=unknown`; storage migration journals не переписываются. Baseline блока 1 ниже остаётся историческим снимком схемы до 0014.

## Исполняемый контракт и baseline

Go: `internal/models/storage_recovery.go` и `storage_recovery_test.go`. TypeScript: `web/src/types/storageRecovery.ts`, `web/tests/storageRecovery.test.ts`. Общий JSON fixture: `internal/models/testdata/storage-recovery.json`, включая version выше JS safe integer, неизвестный/нулевой total и различные времена heartbeat/progress.

Baseline SQLite: 13 миграций `0001_init.sql`–`0013_read_model_version.sql`; новых migrations в блоке 1 нет. `torrent_storage` (миграция 0005): `hash`, `version`, `root`, `state`, `manifest`, `error`. Cleanup хранится в `torrents.cleanup_paths` (JSON массив), `cleanup_ready`, `deletion_mode`, `deletion_error`; manifest не содержит числа выполненных unlink. Identity/revision находятся в `torrent_intents` (0009), durable publication versions — в 0013. WAL и пул 10 open / 5 idle остаются неизменными.

Исходная characterization F41-03: `GetAllTorrents` → barrier → `DeleteTorrent(B)` → `validateOwnership(A, staleList)` → `knownStoragePaths(B)` → `CleanupPaths(B)` → `sql.ErrNoRows`. После блока 2 regression проверяет coherent ownership snapshot → barrier → Delete B → version mismatch → свежая проверка и reservation до filesystem effects. `ErrNoRows` не игнорируется; отдельные тесты проверяют re-add, existing alias и publication между preflight и эффектами.
