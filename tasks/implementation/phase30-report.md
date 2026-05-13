# Phase 30: Range-Aware Streaming QoS и Adaptive Prebuffer (Отчет)

## Что Было Сделано

### Backend: Range-aware stream registration

- Добавлен parser HTTP `Range` для stream QoS в `internal/delivery/http/torrserver/stream_range.go`.
- `serveStream` теперь разбирает `Range` до вызова `http.ServeContent` и передает playhead offset в usecase/engine через `StreamOptions`.
- `HEAD`-запросы и invalid/multi-range requests не включают stream QoS overlay.
- Для torrent reader выставлен явный `SetReadahead(defaultPreloadSize)`.

### Backend: Piece-level QoS overlay

- `StreamManager` переведен с file-level stream mode на session-based piece-level overlay.
- Stream/preload sessions хранятся отдельно от пользовательских file priorities.
- Временный QoS применяется через `torrent.Piece(i).SetPriority(torrent.PiecePriorityHigh)` только для рассчитанных windows.
- Cleanup снимает только piece-level overlay, не меняя `mt.filePriorities`.
- При cleanup учитываются overlay ranges других active streams/preloads той же раздачи, чтобы не снять priority, который еще нужен другому session.
- Debounce при завершении stream сохранен: overlay удерживается до истечения `DebounceDelay`, если новый stream не стартовал раньше.

### Backend: Adaptive prebuffer

- Добавлен расчет stream windows:
  - head prebuffer: первые `20 MiB` или меньше, если файл короче;
  - tail metadata window: последние 4 pieces для `.mp4`, `.mov`, `.m4v`;
  - sliding window впереди playhead;
  - small behind window на 2 pieces перед playhead.
- Окна clipped по границам файла и merge-ятся перед применением.
- Для preload/warmup используется тот же overlay mechanism.

### Backend: Warmup/preload priority safety

- `Engine.Warmup()` больше не вызывает `file.SetPriority(torrent.PiecePriorityHigh)`.
- Warmup регистрирует preload overlay на время чтения и снимает его через cleanup после finish/error/cancel.
- Preload больше не оставляет файл в runtime `High` после завершения.

### Backend: Cache status boundaries

- `GetCacheStatus` теперь безопасно обрабатывает `nil` torrent metadata, negative offset и offset beyond EOF.
- Исправлен расчет contiguous cache bytes: теперь offset внутри piece учитывается точно, без округления до полного piece.
- Расчет вынесен в тестируемый helper `continuousCompleteBytesFromOffset`.

### Тесты

- Добавлен `internal/delivery/http/torrserver/stream_range_test.go`:
  - empty range;
  - open-ended range;
  - closed range;
  - suffix range;
  - probe `bytes=0-0`;
  - EOF/invalid/multi-range cases.
- Добавлен `internal/engine/stream_window_test.go`:
  - head + sliding window;
  - MP4 tail metadata window;
  - small-file merge;
  - range beyond EOF;
  - explicit warmup window size;
  - unaligned file offset;
  - overlay range merge;
  - protection of other file overlays in same torrent.
- Обновлен `internal/engine/cache_emul_test.go`:
  - negative offset clamp;
  - exact bytes from offset inside completed piece;
  - incomplete current piece;
  - EOF boundary;
  - last partial piece;
  - nil metadata safety.
- Обновлен `internal/engine/stream_test.go`:
  - новая сигнатура `AddStream`;
  - skip behavior для `HEAD` и invalid QoS.

## Статус Выполнения DoD

- [x] HTTP `Range` передается в streaming QoS layer как playhead offset.
- [x] Stream mode больше не выставляет `High` всему файлу.
- [x] Stream/preload используют piece-level temporary overlay.
- [x] Adaptive prebuffer включает head, MP4/MOV tail metadata и sliding window впереди playhead.
- [x] Overlay cleanup не снимает priorities, нужные другим active streams/preloads.
- [x] Пользовательские file priorities после stream/preload восстанавливаются точно через `mt.filePriorities` и `applyFilePrioritiesAndDownload`.
- [x] User priority changes во время stream/preload не теряются, потому что overlay не меняет base priorities.
- [x] `Warmup()` и `/stream?...&preload` не оставляют permanent runtime `High`.
- [x] `HEAD`, invalid ranges, completed files и inactive torrent states не создают лишних priority mutations.
- [x] Cache status корректно обрабатывает offset boundaries и не возвращает отрицательные/завышенные значения.
- [x] Добавлены тесты на range parser, piece window calculation, overlay cleanup/protection и cache boundaries.
- [x] `go test ./...` проходит успешно.
- [x] `go build ./cmd/hub` проходит успешно.
- [x] Создан отчет `tasks/implementation/phase30-report.md`.

## Проверки

- `go test ./internal/engine ./internal/delivery/http/torrserver ./internal/usecase` - успешно.
- `go test ./...` - успешно.
- `go build ./cmd/hub` - успешно.
- `git diff --check` - успешно.

## Причины Отхода От Плана

- Полная замена `http.ServeContent` не потребовалась: для QoS достаточно разобрать `Range` до передачи управления `ServeContent`, сохранив стандартное HTTP Range поведение Go.
- Multi-range requests для QoS не приоритизируются и помечаются как `SkipQoS`, чтобы не создавать неверный overlay. Сам HTTP-ответ остается на стороне `http.ServeContent`.
- Отдельный endpoint или UI для diagnostics не добавлялся, так как это не входило в фазу.
