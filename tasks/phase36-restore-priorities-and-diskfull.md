# Фаза 36: Восстановление Приоритетов И Надёжный DiskFull

## Статус И Зависимости

- Статус: реализовано и проверено; DoD закрыт. Отчёт: `tasks/implementation/phase36-report.md`.
- Основание: аудит `tasks/draft/codebase-audit-2026-10-21.md`, **F07, F08**.
- Использовать storage mapping и verified readiness фазы 35, гарантии SQLite и удаления фазы 34.

## Цель И Границы

При startup/Resume пользовательские приоритеты восстанавливаются до допуска загрузки. DiskFull запрещает payload download независимо от файловых приоритетов, stream/preload overlay и torrent readers.

Входит необходимая сериализация admission/restore/disk transitions и сохранение пользовательского намерения. Не входят полный FSM/actor-рефакторинг, redesign очереди F24, все гонки overlays F15 и все остальные находки аудита. Связанные гонки исправляются в объёме, необходимом для доказательства F07/F08; дополнительный scope фиксировать отдельно.

## Контракт Восстановления — F07

1. Единая операция восстановления для startup и Resume DB-only торрента: загрузить запись, layout, metainfo/source и сохранённые file priorities.
2. Создать runtime с запретом payload download. Scheduler, metadata callback, recheck и stream admission не могут разрешить загрузку до завершения инициализации.
3. Загрузить base priorities в engine до активации, включая Normal/High/Skip. При metadata pending сохранить отложенные настройки и применить их после metadata до первой payload-загрузки; ошибки не игнорировать.
4. Учитывать policy/deletion/migration/verification state предыдущих фаз и disk gate; только затем разрешить переход в очередь/активную загрузку.
5. При ошибке metadata/БД/инициализации сохранять пользовательские настройки и оставлять запрет загрузки; не объявлять Resume успешным и не сохранять Normal вместо Skip.

- Источник истины для base priorities — сохранённое пользовательское намерение. SyncWorker сохраняет runtime-статистику, но не заменяет настройки значениями по умолчанию из неполного runtime snapshot.
- Stream/preload overlays и временное обнуление при DiskFull не меняют base priorities в памяти/SQLite.
- Paused после рестарта не начинает скачивание; явный Resume восстанавливает те же настройки. Не требуется автоматически делать paused-торренты доступными для streaming: F22 отдельной целью не является.
- Проверить snapshot, формируемый сразу после restore: он не должен затереть приоритеты/файловые записи до появления metadata.

## Контракт DiskFull — F08

### Общий Gate Загрузки

- Единая проверка разрешения payload download: инициализация завершена, пользователь разрешил работу, нет deletion/migration block, проверка диска разрешает запись и нет защёлкнутого DiskFull.
- Использовать engine/library запрет уровня `DisallowDataDownload()` либо эквивалент с доказанным эффектом на readers и overlays; обнуления file priorities недостаточно.
- Все пути Allow/Resume, scheduler, metadata callbacks, recheck и QoS обязаны уважать gate. Не держать взаимоисключающие engine/library locks в обратном порядке.
- Не обещать отмену уже начатого системного write: gate прекращает новые payload-запросы; in-flight операции ограниченно завершаются/отменяются по контракту библиотеки. После ENOSPC не допускаются бесконечные повторы записи.

### Порог, Ошибки И Восстановление

- До первой записи, при Resume и периодически проверять доступное место в фактическом filesystem payload. Если возможны разные filesystem, проверять каждый затронутый root.
- При свободном месте ниже настроенного порога блокировать все требующие payload-загрузки торренты, включая Queued и stream-driven загрузку; выставлять DiskFull с причиной. Не переводить вручную Paused в активное состояние.
- Обрабатывать реальный ENOSPC из storage writer даже между периодическими проверками; сохранять диагностируемую ошибку и запрет загрузки.
- Ошибка Statfs — не «места достаточно»: блокировать новый/возобновляемый payload download, сохранять прежний DiskFull и показывать причину неизвестного состояния диска. Не выдавать ошибку измерения за доказанный ENOSPC.
- DiskFull защёлкивается до явного Resume после успешной проверки места. Освобождение места, рестарт, scheduler или новый stream не снимают блокировку автоматически. Это соответствует исходному ТЗ.
- Resume при нехватке места/ошибке измерения возвращает конфликт с понятной причиной и не очищает блокировку. Повторный успешный Resume идемпотентен и учитывает лимит фоновых загрузок.
- Сохранять блокировку после рестарта и отличать её от ручной паузы; при необходимости добавить миграцию desired state/block reason, не полагаться на периодический snapshot как единственную гарантию.
- Чтение уже verified данных по disk fast path и seeding без payload-записи допускаются при DiskFull. Запрос недостающих данных не должен снять gate; не оставлять плеер бесконечно ждать без причины/отмены.
- User priorities сохраняются во время DiskFull и восстанавливаются после разрешённого Resume; stream refcount/debounce не сбрасываются ради снятия запрета.
- Отдельно описать служебные записи SQLite/completion/logs: нельзя гарантировать их сохранение на полностью заполненном диске. Ошибка persist не должна снять runtime gate, а startup повторно проверяет диск до записи payload.

## API И Документация

- Сохранить действующий endpoint Resume, добавить явный конфликт 409 при disk gate; отсутствующий торрент — 404, ошибка БД — 500. Не возвращать ложный успех.
- В REST/SSE и Web GUI видны DiskFull/причина блокировки. При ошибке измерения использовать отдельную причину, а не «диск заполнен» без доказательства.
- README: ручное возобновление после освобождения места, поведение активного streaming и сохранение file priorities. CORS `*` и TorrServer-совместимость сохранить.

## План Работ И Файлы

1. Regression-тест Resume paused-торрента после закрытия/повторного создания engine и БД, с High/Skip и metadata pending.
2. Объединить startup/Resume restore в `internal/usecase/torrent_uc.go`, `internal/engine/engine.go`, `metainfo.go`, `priority.go`; отделить сохранение настроек от runtime snapshots в repository/SyncWorker.
3. Ввести единый download gate в engine/resource manager, подключить stream/preload, recheck и storage ENOSPC callback. Сериализовать изменения запрета с разрешением загрузки.
4. При необходимости добавить миграцию сохранённых block reasons/desired state; обновить HTTP-ошибки и отображение диагностики в UI.
5. Добавить fake disk probe/fault-injection writer и детерминированные concurrency-тесты; не заполнять реальный диск тестовой машины.
6. Обновить README и создать после реализации `tasks/implementation/phase36-report.md` с DoD, результатами и ограничениями.

## Тестирование

- Startup и Resume для active/paused/DB-only: Normal/High/Skip переживают рестарт; первая payload-операция видит правильные priorities. Metadata callback не включает Normal до применения отложенных настроек.
- SyncWorker до/после metadata, stream/preload, повторный Resume и ошибки БД не сбрасывают сохранённые настройки.
- Нехватка места до старта, в Queued, во время фоновой загрузки и активного stream/preload; reader/overlay не обходят запрет.
- Реальный ENOSPC через fault injection между проверками; ошибка Statfs; пограничные значения порога; отсутствие retry storm и утечек goroutines/readers.
- Освобождение места без Resume не запускает загрузку; рестарт сохраняет latch; Resume при плохом диске неуспешен, после восстановления успешен с прежними priorities.
- Verified direct read/download и разрешённое seeding работают без payload-записи; запрос недостающих данных завершается/отменяется диагностируемо.
- Управляемые barriers для Pause/Resume/scheduler/metadata/DiskFull и stream admission; assertions проверяют не только конечный state, но и отсутствие запрещённых Allow/download событий.
- Delete/Close во время restore/verification/error callback, bounded shutdown, отсутствие deadlock и data race; проверка регрессий фаз 33–35.
- `go test ./...`, `go test -race ./internal/... ./cmd/...`, `go vet ./...`, `go build ./cmd/hub`, `npm run build` в `web/`, `git diff --check`. Если меняется форк — отдельно тесты/race затронутых пакетов вложенного модуля.

## Definition Of Done

- [x] F07: startup/Resume используют общую инициализацию, priorities загружены до payload admission и переживают metadata pending/рестарт.
- [x] SyncWorker, DiskFull и QoS не перезаписывают пользовательские base priorities runtime defaults/overlays.
- [x] F08: DiskFull блокирует новые payload-запросы независимо от readers и overlays; ENOSPC и неизвестное свободное место обработаны безопасно.
- [x] Все пути разрешения загрузки проверяют общий gate; детерминированные тесты подтверждают отсутствие обхода через конкурентные callbacks.
- [x] Освобождение места/рестарт не возобновляют DiskFull автоматически; успешный ручной Resume требует проверки места и сохраняет priorities.
- [x] Ошибки и причины доступны API/UI без ложного успеха; чтение готовых verified данных не требует разрешения payload-загрузки.
- [x] Тесты/race/vet/build проходят; in-flight writes и служебные записи имеют явно описанные пределы гарантий.
- [x] README обновлён; создан `tasks/implementation/phase36-report.md` с фактическими проверками и отклонениями.
