# Phase 25 Implementation Report: WebGUI HTTP Downloads

## Что Было Сделано
- Добавлены HTTP endpoints для скачивания раздачи и отдельного файла:
  - `GET/HEAD /api/v1/torrent/{hash}/download`
  - `GET/HEAD /api/v1/torrent/{hash}/file/{index}/download`
- Реализовано скачивание полностью готового отдельного файла напрямую с диска через `http.ServeContent` с поддержкой `Range` и resume.
- Реализовано скачивание всей раздачи:
  - однофайловая раздача отдается как исходный файл;
  - многофайловая раздача отдается как ZIP-архив, формируемый на лету без сжатия (`Store`) для высокой скорости локального скачивания.
- Добавлена безопасная логика разрешения путей внутри `downloadDir` с учетом текущих вариантов хранения файлов.
- Добавлена защита от небезопасных путей (`..`, абсолютные пути, выход за пределы `downloadDir`).
- Добавлен `Content-Disposition` в CORS exposed headers.
- В Web GUI добавлена кнопка скачивания раздачи в меню торрента.
- В Web GUI добавлена кнопка скачивания файла во вкладке `Files` нижней панели.
- Кнопки скачивания отображаются только для полностью скачанного контента.
- Добавлены backend-тесты для скачивания файла, `Range`, ZIP, незавершенного контента и unsafe path.

## Измененные Файлы
- `internal/delivery/http/api/handlers.go`
- `internal/delivery/http/api/download_handlers.go`
- `internal/delivery/http/api/handlers_test.go`
- `internal/delivery/http/router.go`
- `internal/usecase/torrent_uc.go`
- `web/src/api/client.ts`
- `web/src/components/TorrentTable.vue`
- `web/src/components/BottomPanel.vue`
- `tasks/phase25-webgui-http-downloads.md`

## Статус DoD
- [x] В Web GUI есть кнопка скачивания полностью скачанной раздачи.
- [x] В Web GUI есть кнопка скачивания полностью скачанного файла.
- [x] Кнопки скачивания не отображаются для незавершенных файлов и раздач.
- [x] Backend также запрещает скачивание незавершенного контента через прямой URL.
- [x] Отдельный файл и однофайловая раздача отдаются с диска через `http.ServeContent` и поддерживают HTTP resume (`Range`).
- [x] Многофайловая раздача отдается как потоковый ZIP с сохранением структуры папок.
- [x] Resume для потокового ZIP явно не поддерживается, `Range` к ZIP возвращает `416`.
- [x] Реализована защита от выхода за пределы `downloadDir` при чтении файлов с диска и формировании ZIP.
- [x] `Content-Disposition` доступен через CORS exposed headers.
- [x] Все backend-тесты проходят (`go test ./...`).
- [x] Frontend успешно собирается (`npm run build`).
- [x] Отчет о реализации создан в `tasks/implementation/phase25-report.md`.

## Проверки
- `go test ./...` — успешно.
- `npm run build` в директории `web` — успешно.

## Отходы От Плана
- Отходов от согласованного плана нет.
- Для потокового ZIP `Content-Length` не выставляется, а `Range` явно отклоняется ответом `416`, так как восстановление загрузки для ZIP было исключено из требований.
- ZIP формируется без сжатия, чтобы избежать CPU bottleneck на слабых устройствах и NAS.
