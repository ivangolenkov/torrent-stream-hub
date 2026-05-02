# Отчет по фазе 10: Swarm maintenance и восстановление скорости без port-forward

## Что было сделано

- Добавлены настройки swarm watchdog через ENV/flags.
- Повышены defaults peer acquisition для более активного outgoing-only режима.
- Добавлен runtime swarm maintenance state для managed torrent-ов.
- Реализован `SwarmRefreshMonitor`, который периодически проверяет swarm health.
- Добавлена degraded decision logic для low peers, low seeds и stalled download speed.
- Для degraded torrent-ов выполняется rate-limited refresh:
  - temporary connection boost через `SetMaxEstablishedConns`;
  - `AllowDataDownload`;
  - `AllowDataUpload`, если upload не отключен;
  - повторный `DownloadAll` для incomplete torrent-ов с metadata;
  - async DHT announce через доступные DHT servers.
- Расширен `GET /api/v1/health/bt` новыми полями watchdog/degraded/boosted diagnostics.
- Обновлена Web GUI страница `/health/bt`: статусы `Healthy`, `Degraded`, `Boosted`, last refresh time/reason.
- Обновлены `docker-compose.yml` и пример ENV в `torrent-stream-hub-tz-v3.md`.
- Добавлены unit-тесты degraded decision logic и обновлены config/API/engine tests.

## Измененные файлы

- `internal/config/config.go`
- `internal/config/config_test.go`
- `internal/engine/engine.go`
- `internal/engine/config_test.go`
- `internal/engine/swarm_test.go`
- `internal/models/torrent.go`
- `internal/delivery/http/api/handlers_test.go`
- `web/src/types/index.ts`
- `web/src/components/BTHealthPanel.vue`
- `docker-compose.yml`
- `torrent-stream-hub-tz-v3.md`

## Статус выполнения DoD

- [x] Добавлены ENV/flags swarm watchdog-а и aggressive peer acquisition defaults.
- [x] `docker-compose.yml` обновлен новыми значениями.
- [x] `torrent-stream-hub-tz-v3.md` обновлен примером Phase 10 config.
- [x] Engine периодически проверяет swarm health и rate-limited refresh-ит degraded torrents.
- [x] Refresh не блокирует engine lock во время DHT announce и не создает announce storm.
- [x] Incomplete torrents при деградации получают boost connections и дополнительный DHT announce, если DHT доступен.
- [x] Completed seeding torrents не считаются stalled из-за отсутствия download speed.
- [x] BT Health показывает degraded/boosted/last refresh diagnostics.
- [x] Web GUI показывает новые поля на отдельной странице `/health/bt` в общем светлом стиле.
- [x] Диагностика и логи не раскрывают IP/ports peers.
- [x] Добавлены или обновлены релевантные тесты.
- [x] `go test ./...` проходит.
- [x] `(cd web && npm run build)` проходит.
- [x] `CGO_ENABLED=0 go build ./cmd/hub` проходит.

## Причины отхода от плана

- Принудительный tracker announce через private API `anacrolix/torrent` не добавлялся. Используются публичные механизмы: повышенные watermarks, connection boost и дополнительный DHT announce.
- Проверка фактической внешней reachable/connectable status не реализована: без внешнего probe это нельзя надежно определить локально.

## Проверки

- `go test ./...`
- `(cd web && npm run build)`
- `CGO_ENABLED=0 go build ./cmd/hub`
