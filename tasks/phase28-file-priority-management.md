# Phase 28: Управление приоритетами файлов и раздач

## Описание
В рамках этой фазы необходимо реализовать возможность управления приоритетом скачивания как для всей раздачи (массовое применение ко всем файлам), так и для отдельных файлов. Поддерживаемые приоритеты: Не скачивать (-1 / None), Обычный (0 / Normal), Высокий (1 / High). Управление должно быть доступно из Web GUI.

## Задачи

### 1. Backend: Data Layer & Engine
- Добавить метод `UpdateFilePriority(hash string, fileIndex int, priority models.FilePriority) error` в `internal/repository/torrent_repo.go`.
- Добавить метод `UpdateTorrentFilesPriority(hash string, priority models.FilePriority) error` в `internal/repository/torrent_repo.go`.
- Реализовать метод `SetFilePriority(hash string, fileIndex int, priority models.FilePriority) error` в `internal/engine/engine.go`. Метод должен находить файл через `anacrolix/torrent`, вызывать `f.SetPriority()` и сохранять статус в БД.
- Реализовать метод `SetTorrentFilesPriority(hash string, priority models.FilePriority) error` в `internal/engine/engine.go` (массовое применение для всех файлов раздачи).

### 2. Backend: API & UseCase
- Добавить методы для установки приоритетов в `internal/usecase/torrent_uc.go`.
- Реализовать HTTP-хендлеры в `internal/delivery/http/api/handlers.go` и зарегистрировать их в `router.go`:
  - `PUT /api/v1/torrent/:hash/priority` (в JSON теле ожидается `{"priority": <int>}`)
  - `PUT /api/v1/torrent/:hash/file/:index/priority` (в JSON теле ожидается `{"priority": <int>}`)

### 3. Frontend: API Client & Store
- Обновить класс API-клиента (`web/src/api/client.ts`), добавив методы `setTorrentPriority(hash: string, priority: number)` и `setFilePriority(hash: string, index: number, priority: number)`.
- Добавить соответствующие Actions в `web/src/stores/torrentStore.ts` для изменения приоритетов с последующим обновлением данных (fetchTorrents/fetchFiles).

### 4. Frontend: UI Components
- **Файлы (`web/src/components/BottomPanel.vue`)**: Во вкладке "Files" добавить новую колонку "Priority". Разместить в ней элементы управления (например, селект или группу кнопок) для каждого файла с вариантами: Не качать (None), Обычный (Normal), Высокий (High).
- **Раздачи (`web/src/components/TorrentTable.vue`)**: В основной таблице торрентов добавить новую колонку "Priority" с аналогичными элементами управления для массового изменения приоритета всех файлов выбранной раздачи.

## Definition of Done (DoD)
- [ ] Методы обновления приоритетов добавлены в слой репозитория и корректно сохраняют данные в SQLite.
- [ ] В `engine.go` реализовано применение приоритетов на уровне `anacrolix/torrent` (вызов `f.SetPriority()`).
- [ ] Написаны unit-тесты (или обновлены существующие) для слоя бизнес-логики/репозитория.
- [ ] API-эндпоинты (PUT) добавлены и корректно обрабатывают запросы.
- [ ] Во вкладке "Files" (BottomPanel) появилась колонка "Priority", смена приоритета файла работает.
- [ ] В главной таблице (TorrentTable) появилась колонка "Priority", смена приоритета массово меняет настройки всех файлов раздачи.
- [ ] GUI отзывчив, корректно отображает текущие приоритеты и обновляет их без необходимости ручного обновления страницы.