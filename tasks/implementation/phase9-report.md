# Отчет по фазе 9: Оптимизация BitTorrent engine core и диагностика swarm health

## Что было сделано

- Добавлены настройки BitTorrent engine через ENV/flags: seeding, upload, DHT, PEX, UPnP, TCP, uTP, IPv6, profile, retrackers, peer connection limits и dial rate.
- Добавлены defaults для ручного создания `config.Config`, чтобы тесты и runtime использовали одинаковую baseline-конфигурацию.
- `anacrolix/torrent.ClientConfig` теперь получает явные настройки seeding/upload/discovery/connection acquisition.
- qBittorrent-like profile включен по умолчанию: user-agent, extended handshake version, BEP20 prefix и randomized PeerID.
- Добавлен public retracker augmentation для magnet, bare infohash и `.torrent` через `TorrentSpec`.
- Добавлена загрузка дополнительных retrackers из `/config/trackers.txt` без сетевых запросов на старте.
- Добавлен backend model и endpoint `GET /api/v1/health/bt` для BitTorrent health diagnostics.
- Добавлена compact BT Health panel в Web GUI.
- Обновлен `docker-compose.yml` с новыми BT ENV-настройками.
- Обновлен пример конфигурации в `torrent-stream-hub-tz-v3.md`.
- Добавлены тесты config, engine retrackers/client config и API health endpoint.

## Измененные файлы

- `internal/config/config.go`
- `internal/config/config_test.go`
- `internal/engine/engine.go`
- `internal/engine/config_test.go`
- `internal/models/torrent.go`
- `internal/usecase/torrent_uc.go`
- `internal/delivery/http/api/handlers.go`
- `internal/delivery/http/api/handlers_test.go`
- `web/src/types/index.ts`
- `web/src/api/client.ts`
- `web/src/components/BTHealthPanel.vue`
- `web/src/views/Home.vue`
- `docker-compose.yml`
- `torrent-stream-hub-tz-v3.md`

## Статус выполнения DoD

- [x] `clientConfig.Seed` включен по умолчанию, upload не отключен по умолчанию.
- [x] qBittorrent-like profile включен по умолчанию и применен к `anacrolix/torrent.ClientConfig`.
- [x] Public retrackers добавляются по умолчанию для magnet, bare infohash и `.torrent` add flow.
- [x] DHT и PEX включены по умолчанию и видны в diagnostics.
- [x] Connection acquisition параметры стали настраиваемыми через ENV.
- [x] Завершенные torrent-ы остаются в engine и готовы к seeding.
- [x] Web GUI показывает BT health diagnostics и warning про возможное отсутствие входящей connectivity без port-forward.
- [x] Диагностика не раскрывает IP/ports конкретных peers.
- [x] `docker-compose.yml` содержит новые ENV-настройки BT engine.
- [x] Добавлены или обновлены релевантные тесты.
- [x] `go test ./...` проходит.
- [x] `(cd web && npm run build)` проходит.
- [x] `CGO_ENABLED=0 go build ./cmd/hub` проходит.

## Причины отхода от плана

- Сетевая загрузка tracker list намеренно не добавлялась: используются встроенные retrackers и локальный `/config/trackers.txt`, чтобы старт сервиса не зависел от внешнего HTTP-запроса.
- Проверка фактической внешней connectability без port-forward не реализована как активный probe. UI показывает conservative warning, потому что сервис может работать за NAT, а UPnP не гарантирует успешный проброс.

## Проверки

- `go test ./...`
- `(cd web && npm run build)`
- `CGO_ENABLED=0 go build ./cmd/hub`
