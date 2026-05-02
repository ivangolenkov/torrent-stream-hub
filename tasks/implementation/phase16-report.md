# Фаза 16: Non-destructive peer discovery recovery — отчет

## Что было сделано

- Убран destructive automatic recovery path из `checkSwarms()`:
  - обычная swarm degradation больше не планирует full BT client recycle;
  - обычная swarm degradation больше не запускает automatic hard refresh (`Drop()` + re-add), даже если флаг случайно включен.
- Добавлен non-destructive peer discovery refresh:
  - temporary `SetMaxEstablishedConns(HUB_BT_SWARM_BOOST_CONNS)`;
  - `AllowDataDownload()`;
  - `AllowDataUpload()` если upload разрешен;
  - `DownloadAll()` для incomplete torrent с готовой metadata;
  - DHT announce через существующие DHT servers;
  - повторное добавление retrackers через `AddTrackers()` без `Drop()`.
- Смягчена `decideSwarmHealth()`:
  - активная загрузка выше stalled threshold не считается unhealthy только из-за peer/seed thresholds;
  - active stream с поступающими данными не провоцирует degraded decision;
  - metadata pending получает grace period перед degraded state.
- Добавлена диагностика peer refresh:
  - `last_peer_refresh_at`;
  - `last_peer_refresh_reason`.
- Добавлена диагностика piece completion storage:
  - `piece_completion_backend`;
  - `piece_completion_persistent`;
  - `piece_completion_error`.
- Storage теперь создается через явный `storage.NewBoltPieceCompletion()` + `storage.NewFileWithCompletion()`.
- Если bolt completion открыть не удалось, fallback в memory остается возможным, но больше не silent: warning попадает в logs и `/health/bt`.
- Диагностические ошибки recycle/hard refresh/restore теперь sanitizing + truncation, чтобы не отдавать огромные списки файлов в health response.
- Обновлен Web UI `/health/bt`:
  - текст recovery описывает non-destructive DHT/tracker refresh;
  - client recycle/hard refresh обозначены как manual advanced diagnostics;
  - добавлен блок Piece Completion.
- Обновлены/добавлены unit tests для swarm decision:
  - active download не degraded из-за low peers;
  - active download не degraded из-за low seeds;
  - low peers still degraded если загрузка не активна;
  - peer trend drop игнорируется при активной скорости;
  - metadata pending grace не degraded.

## Статус выполнения DoD

- [x] Ordinary swarm degradation no longer triggers automatic torrent drop/re-add.
- [x] Ordinary swarm degradation no longer triggers automatic full BT client recycle.
- [x] Peer discovery refresh is non-destructive and uses DHT/tracker/boost operations only.
- [x] Working torrent with metadata and active speed is not classified as unhealthy solely because connected peers are below a fixed threshold.
- [x] Metadata-ready torrent does not become metadata-less because of automatic recovery.
- [x] Manual recycle/hard refresh remain available as advanced diagnostics and are blocked by active streams.
- [x] BT Health explains peer refresh and recycle state without peer IP/ports.
- [x] Piece completion backend status is visible in diagnostics.
- [x] Created report `tasks/implementation/phase16-report.md` after implementation.
- [x] Successful checks:
  - `go test ./...`
  - `(cd web && npm run build)`
  - `CGO_ENABLED=0 go build ./cmd/hub`

## Причины отхода от плана

- Отдельный ENV `HUB_BT_CLIENT_AUTO_RECYCLE_ENABLED` не добавлялся. Вместо этого automatic recycle полностью удален из swarm recovery path, а существующий `HUB_BT_CLIENT_RECYCLE_ENABLED` продолжает управлять manual endpoint. Это минимальнее и сохраняет совместимость с текущим UI/API.
- Piece completion fallback в memory не превращен в startup fatal error. Для локального NAS/контейнерного запуска безопаснее явно диагностировать fallback в `/health/bt`, чем полностью блокировать старт сервиса из-за временного lock/permission issue.
