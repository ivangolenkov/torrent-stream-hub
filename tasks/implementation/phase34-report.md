# Фаза 34: Безопасный Доступ К Файлам И Очистка Конфигурации — Отчёт

Дата: 2026-10-22. Основание: `tasks/phase34-storage-safety-and-config-cleanup.md`, F01/F02/F03/F04/F10 из аудита. Перед реализацией проверены завершённый DoD фазы 33 и её отчёт; её regression-тесты входят в итоговые прогоны. Фазы 35–36 не реализовывались.

## Что Сделано

### F01/F02: Общие Файловые Гарантии

- `internal/payload/files.go`: общий layout provider на базе library `DefaultFilePathMaker`, относительные пути, запрет symlink/нештатных типов, фактическое открытие через `os.Root`, проверка regular file по descriptor и файловой идентичности при открытии.
- `internal/usecase/temporary_files.go`: единый deletion workflow для ручного удаления и FIFO, точные manifests из runtime/cached metainfo с проверкой infohash. Общая проверка protected paths, других владельцев, `.part`, case/unicode/hardlink aliases. Удаляются только manifest entries и пустые родительские каталоги.
- `internal/usecase/torrent_uc.go`: удалены guessed-path candidates и `os.RemoveAll` fallback. DB-only запись без доказуемого layout не разрешает файловые операции. Перед ручным удалением проверяются lease, stream activity и debounce.
- Переиспользованы существующие durable cleanup intent/ready manifest и Paused из фазы 33; новая SQL-миграция для этого не понадобилась. После частичной ошибки повторный Delete продолжает manifest, отсутствие уже удалённых файлов допустимо.
- `temporary.go`, `torrent_uc.go`, `sync_worker.go`: pending cleanup запрещает новые передачи, Resume, повторный add до engine mutation и startup restore. SyncWorker не перезаписывает сохранённое намерение runtime-снимком. Закрепление не отменяет уже записанный manifest; ручной Delete остаётся доступен.
- Download descriptor заменён с абсолютного `DiskPath` на безопасную операцию `Open()`. Preflight проверяет точный metadata path, metadata length, сохранённую готовность, отсутствие storage `.part`, ownership и реальный файл. Каждое последующее открытие повторяет ownership/root-based проверки под lifecycle lock.
- `internal/delivery/http/api/download_handlers.go`: single-file/ZIP больше не делают `os.Open` по ранее проверенному абсолютному пути. Manifest/имена ZIP проверяются до заголовков, entries открываются безопасно по одному без накопления descriptor. ZIP завершается central directory только при полном успехе; контекст отменяет копирование. HEAD/Range/attachment и transfer leases сохранены.
- Native HTTP: отсутствие файла/торрента — 404, неподтверждённый layout/занятость — 409, ошибки хранилища/БД — общий 500 без абсолютных путей. `api/handlers.go` и TorrServer `rem` возвращают конфликт при использовании торрента; drop/wipe сохраняют прежние no-op контракты.
- `internal/engine/stream_source.go`: direct streaming и проверка готовности используют общий layout/root-open helper, сохраняя существующие readiness/fallback и lifecycle-проверки.

### Дополнительный Barrier В Storage Форка

Повторные тесты выявили возможность позднего `MarkNotComplete`/переименования после Drop: `fileTorrentImpl.Close()` прежде был no-op, а completion-операция может выполняться вне client lock.

В `pkg/anacrolix-torrent/storage/file-torrent.go`, `file-piece.go`, `file-torrent-io.go` добавлен lifecycle barrier: Close ждёт начатые completion/write операции, после Close новые MarkComplete/MarkNotComplete/WriteAt/Completion отклоняются с `os.ErrClosed`. В `file-client.go` инициализация переведена на именованные поля.

Это узкое необходимое дополнение к безопасному удалению F01, а не исправление всей F16 или реализация F06. Доверие к completion по содержимому, изоляция layout и общий engine lifecycle остаются задачами следующих фаз.

### F03: Удаление Неработающей Авторизации

- Из `internal/config/config.go` удалены auth-поля, env и CLI-флаги.
- README, Compose и актуальные auth-примеры/описание router в `torrent-stream-hub-tz-v3.md` очищены. Makefile не содержал таких настроек.
- README сохраняет предупреждение об отсутствии встроенной авторизации, сетевой доступности API/логов и защищённом reverse proxy; есть upgrade note об удалённых опциях. Старые CLI отклоняются, env больше не читаются.
- Исторические отчёты/аудит, маскирование Authorization и tracker authentication не переписывались. CORS `*` не менялся.

### F04: SQLite

- `internal/repository/sqlite.go`: корректное объединение DSN query через `net/url`, modernc `_pragma` для каждого нового соединения: `busy_timeout(5000)`, `foreign_keys(1)`, WAL файловой БД.
- Проверяется фактический WAL; любая ошибка инициализации закрывает `sql.DB` через defer. Ограниченный пул сохраняется.
- Для `:memory:` и memory URI задан один постоянный connection; journal — `memory`, без требования недоступного WAL.

### F10: Web GUI

- `TorrentTable.vue` сохраняет currentTarget до await, проверяет актуальность hash/кнопки и connected element, закрывает меню при исчезновении торрента.
- Добавлены минимальные Vitest/jsdom/Vue Test Utils, `web/vitest.config.ts`, `web/tests/TorrentTable.test.ts` и `npm test`; обновлён lockfile. Vue Test Utils закреплён на 2.4.6 для совместимости с используемым Node 22.15.

## Проверки

Успешно выполнены на macOS arm64:

```sh
go test -count=1 ./...
go test -race -count=1 ./internal/... ./cmd/...
go vet ./...
go build -o <approved-temporary-directory>/phase34-hub ./cmd/hub
go test -count=10 ./internal/usecase -run 'TestManual|TestDownloadRevalidates'
# Из pkg/anacrolix-torrent:
go test ./storage -count=1
go test . ./storage -run 'TestFileReadyForDirectRead|TestFile.*Path|Test.*Completion|TestFileClose' -count=1
go test -race . ./storage -run 'TestFileReadyForDirectRead|TestFile.*Path|Test.*Completion|TestFileClose' -count=1
# Из web:
npm test
npm run build
```

`git diff --check` проходит. Как и раньше, macOS race-линковка сообщает `malformed LC_DYSYMTAB`; race-прогоны успешны.

Новые/обновлённые тесты:

- `internal/usecase/storage_safety_test.go`, `torrent_uc_test.go`: точное single/multi-file удаление, final/part, сохранность одноимённого альтернативного пути и постороннего вложенного файла, отказ от рекурсивного удаления неизвестного layout, lease/debounce и конкурентный admission/delete, сбой финализации manifest в SQLite, рестарт/retry и запрет re-add/Resume/transfer при pending deletion.
- Preflight/reopen: подмена leaf/родительского каталога symlink, directory, размер, `.part`, missing file, shared manifest, hardlink другого payload и служебной completion DB. Старые DB-only guessed-path fixtures заменены на реальные matching metainfo либо тест отказа.
- `internal/delivery/http/api/storage_safety_test.go`, `handlers_test.go`: GET/HEAD для single-file/torrent-download, 404/409, отсутствие утечки абсолютного пути, Range, ZIP, подмена второго ZIP entry после начала HTTP-ответа без выдачи внешнего файла и без ложного завершения ZIP.
- `internal/payload/files_test.go`: traversal, абсолютные пути, non-regular file, внутренний symlink и обычный root-open.
- `internal/repository/sqlite_test.go`: восемь одновременно удерживаемых соединений, PRAGMA на каждом и на новом физическом соединении после сброса idle pool; FK violation, ожидание занятой БД до освобождения lock, memory DSN и invalid query. Существующие миграционные/repository тесты также проходят.
- `internal/config/auth_removed_test.go`: старые auth-флаги не зарегистрированы/не принимаются при установленных legacy env.
- `pkg/anacrolix-torrent/storage/file-close_test.go`: управляемая блокировка начатой completion-операции, ожидание Close, отклонение позднего hash-result/write и сохранность нового файла на прежнем пути.
- Frontend: три component-теста обеих action-button templates, actual DOM dispatch/currentTarget после await, повторные/конкурирующие клики, outside click и исчезновение строки. jsdom не моделирует CSS viewport.

Дополнительно через Playwright на Vite с полностью замоканными API/SSE, без пользовательских downloads/config: меню открывается/закрывается на viewport 1280×800 и 390×844, находится внутри viewport, JS page errors отсутствуют. Временный dev server остановлен.

## Статус Definition Of Done

- [x] F01: точные manifests и общий root-contained deletion, без guessed paths/рекурсивного fallback.
- [x] Ошибки/незавершённое удаление безопасно повторяются после рестарта; активные передачи и debounce защищены.
- [x] F02: download/ZIP используют безопасное открытие подтверждённых payload; HEAD/Range сохранены.
- [x] F03: неработающие auth-настройки удалены из кода/актуальных примеров, предупреждение о сетевой доступности сохранено.
- [x] F04: WAL файловой БД, FK/busy timeout на всех pooled connections; закрытие ресурсов при ошибке инициализации.
- [x] F10: меню после await работает на desktop/mobile, автоматические regression-тесты проходят.
- [x] Регрессии фазы 33, CORS `*`, Go/race/vet/build и frontend test/build проходят; ограничения зафиксированы.
- [x] Этот отчёт создан; фаза 35 не начата.

## Ограничения И Отклонения

- Существенных функциональных отклонений от согласованного плана нет. SQL-миграция не понадобилась: intent/ready manifest уже существовали. Добавлен узкий storage Close barrier по выявленному при тестировании условию безопасности F01.
- Root containment не является sandbox против локального привилегированного writer; внешние hardlinks вне зарегистрированного владения и недетектируемые stat-проверкой изменения содержимого не защищены hash-verification этой фазой. Политика verified completion — фаза 35.
- Нельзя гарантировать архивную целостность при изменении содержимого внешним writer во время чтения. Если entry исчез/стал небезопасным после заголовков, передача прерывается, без JSON-дописки и успешного ZIP central directory.
- Неподтверждённые/shared legacy paths и metadata-pending другие владельцы консервативно блокируют операции. Pending deletion не возобновляется для получения metadata: требуется восстановить исходный metainfo и повторить Delete либо явно удалить только запись, сохранив файлы. Это исключает повторное скачивание после намерения удаления.
- Полный набор всех upstream subpackages форка не запускался: исторически дополнительный `storage/possum` требует внешнюю `libpossum`. Затронутый storage package пройден целиком, корневой пакет форка — целевые тесты и race.
- NAS/Smart TV и отдельные Windows/Linux прогоны не выполнялись. Playwright использовал mock API, не проверял реальные torrent actions; backend контракты проверены HTTP/integration tests.
- Дополнительный `npm audit --json` возвращает exit 1: шесть high-severity записей (`vue`, `@vue/server-renderer`, `vite`, `postcss`, `nanoid`, `source-map-js`). Это не вывод об эксплуатируемости приложения. Массовый dependency upgrade/`npm audit fix` не выполнялись вне scope; требуется отдельный разбор. Успешные test/build не означают отсутствие dependency vulnerabilities.
