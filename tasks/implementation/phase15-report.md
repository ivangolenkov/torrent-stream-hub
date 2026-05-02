# Phase 15 Report: Restore/recycle reliability и integrity прогресса

## Что было сделано

- Добавлена фаза `tasks/phase15-restore-recycle-and-progress-integrity.md`.
- Исправлен restore active torrents после restart:
  - persisted metainfo больше не останавливает restore при ошибке;
  - fallback идет через `source_uri` magnet, затем bare infohash;
  - invalid metainfo safely переименовывается в `.invalid`.
- Исправлен recycle BT client:
  - re-add теперь пробует несколько candidates;
  - invalid runtime metainfo не блокирует magnet/infohash fallback;
  - `last_readd_source` показывает фактический source: `metainfo_runtime`, `magnet`, `infohash`.
- Добавлена валидация reusable metainfo:
  - проверяются `InfoBytes`, `UnmarshalInfo`, v2 piece layers;
  - v1 metainfo с `PieceLayers` считается непригодным для re-add;
  - runtime-generated metainfo не перезаписывает уже существующий valid metainfo.
- Защищен persisted progress от отката:
  - `torrents.downloaded = MAX(existing, excluded)`;
  - `files.downloaded = MAX(existing, excluded)`.
- `GetAllTorrents()` теперь возвращает persisted files.
- Добавлена restore diagnostics в BT Health:
  - `last_restore_source`;
  - `last_restore_error`;
  - `invalid_metainfo_count`.
- Web UI `/health/bt` показывает restore source и invalid metainfo count.
- Исправлен lifecycle file storage:
  - `Engine` теперь явно владеет `storage.ClientImplCloser`;
  - `Engine.Close()` закрывает torrent client и storage;
  - BT client recycle закрывает old storage до создания new client, чтобы не оставлять lock на `/downloads/.torrent.bolt.db`.
- Добавлен recovery recheck:
  - если после restore runtime progress меньше persisted `downloaded`, запускается `VerifyDataContext()`;
  - на время recheck file priorities временно отключаются;
  - после recheck `DownloadAll()` запускается повторно для `Downloading` torrents.
- Добавлены tests:
  - `GetAllTorrents` includes files;
  - `SaveTorrent` does not decrease torrent/file progress;
  - restore falls back from invalid metainfo to source URI and renames invalid file.

## Измененные файлы

- `internal/engine/engine.go`
- `internal/models/torrent.go`
- `internal/repository/torrent_repo.go`
- `internal/repository/torrent_repo_test.go`
- `internal/usecase/torrent_uc.go`
- `internal/usecase/torrent_uc_test.go`
- `web/src/components/BTHealthPanel.vue`
- `web/src/types/index.ts`
- `tasks/phase15-restore-recycle-and-progress-integrity.md`

## Статус DoD

- [x] Restart no longer leaves `Downloading` torrents DB-only when magnet/infohash fallback is available.
- [x] Bad persisted metainfo does not block restore/recycle.
- [x] Client recycle does not leave engine empty if fallback source exists.
- [x] Persisted torrent/file progress does not decrease during normal sync.
- [x] `GetAllTorrents()` includes files for DB-only torrents.
- [x] Restore/recycle diagnostics explain source/failure.
- [x] Existing problematic metainfo is safely ignored/renamed, not deleted silently.
- [x] Torrent storage lifecycle closes piece completion DB before recycle/restart re-open.
- [x] Existing partial data is rechecked when runtime progress is behind persisted progress.
- [x] Created report `tasks/implementation/phase15-report.md`.
- [x] `go test ./...`
- [x] `(cd web && npm run build)`
- [x] `CGO_ENABLED=0 go build ./cmd/hub`

## Причины отхода от плана

- Per-torrent persistent `last_restore_error` не добавлялся в модель torrent/SQLite, чтобы не расширять DB schema ради диагностики текущей проблемы. Добавлена global diagnostics в BT Health, достаточная для restart/recycle troubleshooting.
- Invalid metainfo cleanup реализован через rename to `.invalid`. Файл не удаляется безвозвратно.
- Explicit progress reset/recheck operation не добавлялась, потому что это отдельная будущая операция. Обычный sync теперь монотонный по downloaded bytes.
- Recheck реализован как automatic recovery только при detected progress loss после restore/resume, а не как отдельная пользовательская команда.

## Проверки

```bash
go test ./...
npm run build # in web/
CGO_ENABLED=0 go build ./cmd/hub
```

Все проверки успешно прошли. Локальный бинарь `hub`, созданный build-командой, удален.
