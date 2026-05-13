# Phase 30: Range-Aware Streaming QoS и Adaptive Prebuffer

## Описание

В рамках этой фазы необходимо улучшить streaming QoS так, чтобы HTTP `Range`-запросы реально влияли на приоритизацию загрузки. Текущая реализация при старте stream переводит весь выбранный файл в `High` и при необходимости временно понижает другие файлы, что плохо подходит для seek-сценариев и может искажать пользовательские file priorities.

Новая модель должна перейти от file-level stream mode к временной piece-level QoS overlay:

- при `Range`-запросе приоритизировать окно вокруг текущей позиции воспроизведения;
- делать adaptive prebuffer: первые N MB, последние pieces для MP4/MOV metadata, затем sliding window впереди playhead;
- точно восстанавливать пользовательские file priorities после завершения stream/preload;
- не сохранять временные stream/preload priorities в SQLite;
- не менять пользовательские `mt.filePriorities` ради stream mode.

## Текущее Поведение

- `internal/delivery/http/torrserver/handlers.go` получает HTTP `Range`, но передает в `uc.AddStream()` только `hash` и `index`.
- `http.ServeContent` сам обрабатывает `Range`, а engine QoS не знает стартовый offset.
- `internal/engine/stream.go` при stream mode выставляет `High` всему файлу и может временно понизить другие файлы с `High` до `Normal`.
- `internal/engine/engine.go` `Warmup()` выставляет всему файлу `High` и не восстанавливает runtime priority после preload.
- `internal/engine/cache_emul.go` считает cache status от offset, но эта информация пока не используется для приоритизации.

## Не Входит В Фазу

- Полная замена `http.ServeContent` собственным Range-сервером, если это не потребуется для корректной передачи playhead в QoS.
- Изменение схемы SQLite.
- Изменение UI управления file priorities.
- Реализация `HUB_MAX_ACTIVE_STREAMS`, если только это не понадобится как защитный лимит для новых overlay windows.
- Глубокая переработка TorrServer compatibility beyond preload/cache/status behavior.
- Изменение политики resource manager, disk-full и swarm watchdog, кроме корректного взаимодействия с active streams.

## План Работ

### 1. Подготовить Range/Stream Модель

Добавить внутреннюю модель stream request, например `StreamRequest` или `StreamOptions`:

- `RangeStart int64`;
- `RangeEnd int64`;
- `HasRange bool`;
- `IsHEAD bool`;
- `Preload bool`, если понадобится общий путь для preload;
- `SessionID`, если будет выбран session-based учет active streams.

Реализовать lightweight parser для HTTP `Range`:

- поддержать `bytes=start-end`;
- поддержать `bytes=start-`;
- поддержать `bytes=-suffix`;
- безопасно обрабатывать пустой, некорректный и multi-range header;
- не менять фактическое HTTP-поведение `ServeContent`, если parser не смог разобрать header.

Ожидаемый результат:

- `serveStream` может передать в usecase/engine начальный playhead offset до вызова `http.ServeContent`.
- Invalid/multi-range не приводят к permanent QoS side effects.

### 2. Передать Range В UseCase И Engine

Изменить сигнатуры на уровне usecase/engine без изменения внешнего HTTP API:

- `TorrentUseCase.AddStream(ctx, hash, index, options)`;
- `StreamManager.AddStream(ctx, hash, index, options)`.

Правила:

- `HEAD`-запросы не должны включать stream QoS и не должны менять priorities;
- для обычного stream без `Range` использовать offset `0`;
- для валидного `Range` использовать рассчитанный start offset;
- если `AddStream` вернул ошибку, streaming можно оставить текущим behavior: залогировать и продолжить, если файл доступен.

Ожидаемый результат:

- engine получает текущую позицию воспроизведения и может рассчитать piece window.
- Поведение `/stream`, `/stream/{name}` и `/play/{hash}/{id}` остается совместимым.

### 3. Заменить File-Level Stream Mode На Piece-Level QoS Overlay

Переработать `StreamManager`:

- хранить active stream sessions, а не только счетчик по `(hash, fileIndex)`;
- для каждой session хранить current playhead и active boosted piece ranges;
- применять временные priorities через `torrent.Piece(i).SetPriority(...)`, а не через `file.SetPriority(High)`;
- при обновлении окна пересчитывать union ranges по всем active sessions;
- при завершении session снимать только те piece-level priorities, которые больше не нужны другим active stream/preload overlays;
- после cleanup вызывать восстановление base priorities через `applyFilePrioritiesAndDownload(mt)` там, где это безопасно.

Правила восстановления:

- `mt.filePriorities` остается source of truth для пользовательских priorities;
- stream/preload overlay не пишет в `mt.filePriorities`;
- stream/preload overlay не пишет в repository;
- пользовательский `High` у другого файла не должен временно понижаться до `Normal`;
- пользовательский `None` у streamed file может быть временно overridden только active stream overlay, но после stream должен вернуться в `None`.

Ожидаемый результат:

- stream mode больше не выставляет `High` всему файлу;
- active stream приоритизирует только нужные pieces;
- после debounce пользовательские file priorities восстановлены точно.

### 4. Реализовать Adaptive Prebuffer Policy

Добавить helper для расчета prebuffer windows по файлу:

- head window: первые N MB файла;
- tail metadata window: последние M pieces для `.mp4`, `.mov`, `.m4v`;
- sliding window: окно впереди `RangeStart`/playhead;
- optional behind window: 1-2 pieces перед playhead для seek/keyframe stability;
- все окна должны быть clipped по `[file.Offset(), file.Offset()+file.Length())`;
- результат должен быть набором piece ranges `[begin, end)`.

Рекомендуемые defaults для первой реализации:

- head prebuffer: `20 MiB`, согласовано с текущими `ReaderReadAHead` и `PreloadCache`;
- sliding forward window: использовать `cfg.StreamCacheSize`, но ограничить разумным верхним пределом, если потребуется защита от слишком больших piece windows;
- tail metadata: 2-4 pieces для MP4/MOV/M4V.

Ожидаемый результат:

- при старте stream качаются первые N MB и tail metadata для MP4/MOV;
- при seek качается окно вокруг новой позиции;
- чтение через anacrolix reader дополнительно поддерживается `SetReadahead` или `SetReadaheadFunc`.

### 5. Исправить Warmup/Preload

Переработать `Engine.Warmup()` и `/stream?...&preload`:

- убрать `file.SetPriority(torrent.PiecePriorityHigh)` из warmup;
- использовать тот же piece-level overlay, что и stream;
- preload должен уметь прогревать head и tail metadata;
- preload должен снимать свой overlay после завершения, timeout, cancel или error;
- preload state в `TorrServerHandler` должен отражать target/read bytes без permanent priority mutations.

Ожидаемый результат:

- preload больше не оставляет файл в runtime `High`;
- preload и active stream не конфликтуют при cleanup overlay.

### 6. Обновить Cache Status

Уточнить `GetCacheStatus(hash, index, offset)`:

- clamp `offset < 0` к `0` или возвращать `0` без panic;
- `offset >= file.Length()` возвращает `0` bytes remaining;
- не завышать `VirtualCacheBytes`, если offset находится внутри completed piece;
- учитывать, что first/last piece файла могут быть частичными относительно файла;
- оставить лимит `HUB_STREAM_CACHE_SIZE`.

Ожидаемый результат:

- `/cache` возвращает более точную оценку contiguous downloaded bytes вперед от текущего offset;
- cache status и QoS window используют совместимую piece math.

### 7. Обновить User Priority Operations Для Active Overlay

Обновить `SetFilePriority` и `SetTorrentFilesPriority`:

- сначала обновлять base priority в `mt.filePriorities`;
- применять base priority к runtime только с учетом active stream/preload overlay;
- если active overlay есть, не давать user operation случайно снять текущие stream pieces;
- после завершения stream/preload новая user priority должна стать видимой без дополнительного действия.

Ожидаемый результат:

- пользовательские изменения во время stream не теряются;
- stream не портит состояние других файлов;
- bulk priority change во время stream корректно применяется после cleanup.

### 8. Логирование И Diagnostics

Добавить debug/info logs:

- raw `Range` и parsed range;
- stream session start/stop;
- computed head/tail/sliding windows;
- applied overlay ranges;
- cleared overlay ranges;
- debounce cancel/schedule/elapsed;
- priority restore result;
- preload start/finish/cancel/error.

Ожидаемый результат:

- по debug logs можно понять, почему конкретные pieces получили priority и когда overlay был снят.

## Необходимые Тесты

### Unit: HTTP Range Parser

Добавить тесты для helper, который парсит `Range`:

- empty header -> `HasRange=false`, start `0`;
- `bytes=0-` -> start `0`, open end;
- `bytes=100-` -> start `100`, open end;
- `bytes=100-199` -> start `100`, end `199`;
- `bytes=-500` при известном file size -> start `fileSize-500`;
- suffix больше размера файла -> start `0`;
- `bytes=0-0` -> валидный probe range;
- `bytes=999999-` за EOF -> помечается invalid для QoS или clipped без panic;
- `bytes=200-100` -> invalid;
- `items=0-10` -> invalid/ignored;
- `bytes=a-b` -> invalid/ignored;
- `bytes=0-10,20-30` -> multi-range behavior явно зафиксирован: ignored или first range only.

### Unit: Piece Window Calculation

Добавить тесты для расчета windows:

- файл начинается на границе piece;
- файл начинается внутри piece;
- файл заканчивается внутри piece;
- размер файла меньше head prebuffer;
- `RangeStart=0` дает head window;
- `RangeStart` внутри first piece;
- `RangeStart` в середине файла;
- `RangeStart` около конца файла;
- `RangeStart >= file.Length()` не создает windows;
- sliding window clipped по концу файла;
- behind window clipped по началу файла;
- head/tail/sliding windows пересекаются и корректно merge-ятся;
- tail metadata включается для `.mp4`, `.mov`, `.m4v` с разным регистром;
- tail metadata не включается для `.mkv`, `.avi`, `.webm`, `.txt`;
- большой piece length, когда `N MB` меньше одного piece, все равно выбирает минимум 1 piece.

### Unit: Overlay Merge/Cleanup

Добавить тесты для внутреннего manager/helper без реального torrent client, если возможно:

- один stream применяет один window;
- два stream одного файла с пересекающимися windows дают union без дублей;
- два stream одного файла с разными windows не снимают priority друг друга при закрытии одного stream;
- stream разных файлов одной раздачи могут иметь независимые windows;
- cleanup снимает только overlay pieces, которые больше не используются;
- debounce удерживает overlay после disconnect;
- новый stream до истечения debounce отменяет cleanup и пересчитывает window;
- completed file не создает overlay;
- preload overlay и stream overlay пересекаются, cleanup preload не снимает stream window.

### Unit/Integration: Priority Restore

Добавить тесты на восстановление priorities:

- base priorities `Normal/High/None` сохраняются в `mt.filePriorities` во время stream;
- stream streamed file с user `None` временно получает overlay и после cleanup возвращается в `None`;
- другой файл с user `High` не понижается до `Normal` при stream;
- другой файл с user `None` не начинает скачиваться из-за stream;
- user меняет streamed file priority во время active stream, после cleanup остается новое значение;
- user меняет другой file priority во время active stream, изменение не затирается;
- bulk priority change во время active stream применяется как base priority и становится видимым после cleanup;
- paused/error/disk-full/missing-files состояния не восстанавливаются в downloading priorities;
- warmup/preload timeout не оставляет файл в `High`.

### Unit: Cache Status Boundaries

Обновить или добавить тесты для `GetCacheStatus`:

- negative offset;
- offset `0`;
- offset внутри completed first piece;
- offset ровно на границе piece;
- offset в incomplete piece;
- offset около EOF;
- offset `>= file.Length()`;
- fully downloaded file возвращает remaining bytes, а не отрицательные значения;
- результат capped by `StreamCacheSize`;
- first/last partial file pieces не завышают bytes beyond file boundaries.

### HTTP Handler Tests

Добавить тесты на уровень handler/usecase mock, если текущая структура позволяет:

- `GET /stream?...` без `Range` вызывает `AddStream` с offset `0`;
- `GET /stream?...` с `Range: bytes=100-` вызывает `AddStream` с offset `100`;
- `GET /play/{hash}/{id}` корректно мапит TorrServer index и передает range offset;
- `HEAD /stream?...` не вызывает QoS registration;
- invalid range не вызывает panic и не оставляет active stream state;
- `Range: bytes=0-0` не включает большой permanent overlay после завершения request.

### Integration/Manual Verification

После реализации выполнить:

- `go test ./internal/engine`;
- `go test ./internal/usecase`;
- `go test ./internal/delivery/http/...`;
- `go test ./...`;
- `go build ./cmd/hub`.

Ручные проверки на реальном torrent:

- открыть MP4/MOV с начала и убедиться, что head/tail/sliding pieces получают priority;
- сделать seek в середину файла и убедиться, что priority window переехал к новой позиции;
- быстро сделать несколько seek подряд и проверить debounce;
- поставить другой файл в `High`, запустить stream и убедиться, что priority не сброшен;
- поставить streamed file в `None`, запустить stream, закрыть stream и убедиться, что `None` восстановился;
- запустить `/stream?...&preload`, дождаться timeout/finish и убедиться, что файл не остался в runtime `High`.

## Пограничные Кейсы, Которые Должны Быть Учтены

- Нет `Range`: playhead `0`.
- `Range: bytes=0-`: стартовое воспроизведение.
- `Range: bytes=start-end`: окно от `start`.
- `Range: bytes=start-`: окно от `start` до EOF.
- `Range: bytes=-suffix`: окно от `fileSize - suffix`.
- `Range` полностью за пределами файла.
- `Range` с `start > end`.
- Multi-range request.
- Invalid range unit.
- Browser/player probe `bytes=0-0`.
- `HEAD` с Range и без Range.
- Быстрый seek с несколькими короткими HTTP connections.
- Одновременные streams одного файла с разными offsets.
- Одновременные streams разных файлов одной раздачи.
- Пересекающиеся QoS windows.
- Piece пересекает границу двух файлов.
- Файл меньше prebuffer size.
- Файл из одного piece.
- Last piece короче обычного piece.
- Head/tail/sliding windows пересекаются.
- MP4/MOV metadata в конце файла.
- Расширения media file в разном регистре.
- Non-media файл открыт через stream endpoint.
- Metadata еще не готова.
- Torrent удален во время stream.
- Engine закрывается при active stream/debounce/preload.
- Stream context отменен до завершения QoS registration.
- Warmup/preload timeout.
- Warmup/preload error.
- Preload и stream идут одновременно.
- User ставит streamed file в `None` во время просмотра.
- User ставит streamed file в `High` или `Normal` во время просмотра.
- User меняет priority другого файла во время active stream.
- Bulk priority change во время active stream.
- Torrent находится в `Paused`.
- Torrent находится в `DiskFull`.
- Torrent находится в `Error` или `MissingFiles`.
- Streamed file уже полностью скачан.
- `GetCacheStatus` получает negative offset.
- `GetCacheStatus` получает offset beyond EOF.
- Очень большой piece length относительно prebuffer MB.
- Очень маленький configured `StreamCacheSize`.
- Очень большой configured `StreamCacheSize`.
- Много active streams, чтобы overlay не создавал чрезмерное количество boosted pieces.
- `SetReadaheadFunc` не должен вызывать код, который может deadlock под lock anacrolix client.

## Definition Of Done

- HTTP `Range` передается в streaming QoS layer как playhead offset.
- Stream mode больше не выставляет `High` всему файлу.
- Stream/preload используют piece-level temporary overlay.
- Adaptive prebuffer включает head, MP4/MOV tail metadata и sliding window впереди playhead.
- Overlay cleanup не снимает priorities, нужные другим active streams/preloads.
- Пользовательские file priorities после stream/preload восстанавливаются точно.
- User priority changes во время stream/preload не теряются.
- `Warmup()` и `/stream?...&preload` не оставляют permanent runtime `High`.
- `HEAD`, invalid ranges, completed files и inactive torrent states не создают лишних priority mutations.
- Cache status корректно обрабатывает offset boundaries и не возвращает отрицательные/завышенные значения.
- Добавлены тесты на range parser, piece window calculation, overlay cleanup, priority restore и cache boundaries.
- `go test ./...` проходит успешно.
- `go build ./cmd/hub` проходит успешно.
- После реализации создан отчет `tasks/implementation/phase30-report.md` с описанием фактических изменений и статусом DoD.
