# Фаза 9: Оптимизация BitTorrent engine core и диагностика swarm health

## Цель

Улучшить эффективность работы BitTorrent-ядра: ускорить поиск и удержание пиров, включить реальное сидирование, приблизить сетевой профиль к хорошо совместимым клиентам и дать пользователю понятную диагностику состояния swarm/network через Web GUI.

Проблемы, которые должна закрыть фаза:

- После рестарта скорость скачивания выше, чем после длительной работы сервиса.
- Количество пиров со временем не пополняется и чаще только сокращается.
- TorrServer на тех же torrent-ах часто показывает более высокую скорость.
- Upload speed остается нулевым, и неясно, видят ли сервис другие участники swarm.
- При отсутствии port-forward нужно явно показывать, что входящие подключения могут быть ограничены.

## Принятые решения

1. Public retrackers добавляются по умолчанию к magnet, bare infohash и `.torrent` add flow.
2. qBittorrent-like client profile включается по умолчанию для совместимости с трекерами и peers.
3. Реальное сидирование включается по умолчанию: завершенные torrent-ы должны оставаться доступными для upload.
4. Port-forward может отсутствовать; сервис должен работать в degraded incoming connectivity mode и показывать соответствующий warning.
5. BT health diagnostics должны отображаться в Web GUI, без раскрытия IP/port конкретных peers.

## Задачи

### 1. Расширение конфигурации BitTorrent engine

- Добавить ENV/flags в `internal/config/config.go`:
  - `HUB_BT_SEED`, default `true`.
  - `HUB_BT_NO_UPLOAD`, default `false`.
  - `HUB_BT_CLIENT_PROFILE`, default `qbittorrent`.
  - `HUB_BT_RETRACKERS_MODE`, default `append`.
  - `HUB_BT_RETRACKERS_FILE`, default `/config/trackers.txt`.
  - `HUB_BT_DISABLE_DHT`, default `false`.
  - `HUB_BT_DISABLE_PEX`, default `false`.
  - `HUB_BT_DISABLE_UPNP`, default `false`.
  - `HUB_BT_DISABLE_TCP`, default `false`.
  - `HUB_BT_DISABLE_UTP`, default `false`.
  - `HUB_BT_DISABLE_IPV6`, default `false`.
  - `HUB_BT_ESTABLISHED_CONNS_PER_TORRENT`, default `50`.
  - `HUB_BT_HALF_OPEN_CONNS_PER_TORRENT`, default `50`.
  - `HUB_BT_TOTAL_HALF_OPEN_CONNS`, default `500`.
  - `HUB_BT_PEERS_LOW_WATER`, default `100`.
  - `HUB_BT_PEERS_HIGH_WATER`, default `1000`.
  - `HUB_BT_DIAL_RATE_LIMIT`, default `20`.
- Значения должны иметь безопасные fallback defaults, если ENV задан некорректно.
- Логи старта engine должны показывать sanitized snapshot ключевых BT-настроек.

### 2. Настройка `anacrolix/torrent.ClientConfig`

- Применить новые поля конфигурации в `internal/engine/engine.go`:
  - `Seed`.
  - `NoUpload`.
  - `NoDHT`.
  - `DisablePEX`.
  - `NoDefaultPortForwarding`.
  - `DisableTCP`.
  - `DisableUTP`.
  - `DisableIPv6`.
  - `EstablishedConnsPerTorrent`.
  - `HalfOpenConnsPerTorrent`.
  - `TotalHalfOpenConns`.
  - `TorrentPeersLowWater`.
  - `TorrentPeersHighWater`.
  - `DialRateLimiter`.
- Не включать `NoUpload` по умолчанию.
- Не отключать DHT/PEX/TCP/UTP по умолчанию.
- UPnP должен быть разрешен по умолчанию, но при отсутствии port-forward UI должен показывать предупреждение.

### 3. qBittorrent-like client profile

- Для `HUB_BT_CLIENT_PROFILE=qbittorrent` установить:
  - `HTTPUserAgent = "qBittorrent/4.3.9"`.
  - `ExtendedHandshakeClientVersion = "qBittorrent/4.3.9"`.
  - `Bep20 = "-qB4390-"`.
  - randomized `PeerID` с этим prefix.
- Для `HUB_BT_CLIENT_PROFILE=default` оставить стандартный профиль `anacrolix/torrent`.
- Некорректное значение profile должно fallback-иться к `qbittorrent` и логироваться на debug/warn уровне.

### 4. Public retrackers

- Добавить helper для retrackers в engine layer:
  - встроенный список public trackers/retrackers;
  - загрузка дополнительных trackers из `HUB_BT_RETRACKERS_FILE`;
  - дедупликация URL;
  - фильтрация пустых/некорректных строк.
- Поддержать режимы `HUB_BT_RETRACKERS_MODE`:
  - `append`: добавить public retrackers к существующим trackers.
  - `replace`: заменить trackers на public retrackers.
  - `off`: не менять trackers.
- Применить retrackers для:
  - magnet add;
  - bare infohash add;
  - `.torrent` add.
- Не выполнять сетевую загрузку tracker list на старте. Только встроенный список и локальный файл `/config/trackers.txt`.

### 5. Add flow через `TorrentSpec`

- Перевести `AddMagnet` с прямого `client.AddMagnet(magnet)` на flow через `torrent.TorrentSpec`.
- Для `.torrent` использовать `torrent.TorrentSpecFromMetaInfo(metaInfo)` или эквивалентный API текущей версии `anacrolix/torrent`.
- Перед `AddTorrentSpec` применять retracker augmentation.
- Сохранять `SourceURI` так, чтобы restore после restart сохранял trackers из исходного magnet, если они были.
- Bare infohash должен получать retrackers в режиме `append` или `replace`.

### 6. Seeding semantics

- Состояние `StateSeeding` должно соответствовать реальной готовности отдавать данные.
- После достижения 100% не отключать upload и не переводить torrent в пассивное состояние.
- Убедиться, что completed torrent-ы остаются managed в engine и доступны для peer connections.
- В diagnostics явно показывать:
  - seed enabled;
  - upload enabled;
  - upload limit;
  - текущую upload speed.

### 7. BT health diagnostics backend

- Добавить runtime model для BT diagnostics, например `models.BTHealth`.
- Добавить endpoint `GET /api/v1/health/bt`.
- Global diagnostics должны включать:
  - `seed_enabled`.
  - `upload_enabled`.
  - `dht_enabled`.
  - `pex_enabled`.
  - `upnp_enabled`.
  - `tcp_enabled`.
  - `utp_enabled`.
  - `ipv6_enabled`.
  - `listen_port`.
  - `client_profile`.
  - `retrackers_mode`.
  - `incoming_connectivity_note`.
- Per torrent aggregate diagnostics должны включать:
  - `hash`.
  - `name`.
  - `state`.
  - `known`.
  - `connected`.
  - `pending`.
  - `half_open`.
  - `seeds`.
  - `tracker_status`.
  - `tracker_error`.
  - `download_speed`.
  - `upload_speed`.
- Не раскрывать IP/ports peers.

### 8. BT health panel в Web GUI

- Добавить отображение BT health diagnostics в Web GUI.
- Показать global status:
  - DHT/PEX/UPnP/TCP/UTP.
  - Seed/upload state.
  - Client profile.
  - Retrackers mode.
  - Listen port.
- Показать warning:
  - если port-forward неизвестен или отсутствует, входящие peers могут не достигать клиента;
  - это может снижать upload и видимость в swarm.
- Показать per torrent peer health summary без IP/ports.
- UI должен быть compact и не мешать основному torrent dashboard.

### 9. Deployment configuration

- Обновить `docker-compose.yml`, добавив новые ENV со значениями по умолчанию.
- Проверить, что TCP/UDP torrent port остается проброшенным:
  - `50007:50007/tcp`.
  - `50007:50007/udp`.
- При необходимости обновить пример конфигурации в ТЗ.

### 10. Тесты

- Config tests:
  - новые ENV читаются корректно;
  - некорректные значения fallback-ятся к default.
- Engine config tests:
  - `Seed=true` по умолчанию;
  - `NoUpload=false` по умолчанию;
  - qBittorrent profile применяет `HTTPUserAgent`, `ExtendedHandshakeClientVersion`, `Bep20`, `PeerID` prefix;
  - DHT/PEX/TCP/UTP/UPnP defaults включены;
  - connection limit settings применяются.
- Retracker tests:
  - `append` сохраняет existing trackers и добавляет public retrackers;
  - `replace` заменяет trackers;
  - `off` не меняет trackers;
  - local `trackers.txt` читается и дедуплицируется;
  - bare infohash получает trackers при `append`.
- API tests:
  - `GET /api/v1/health/bt` возвращает global diagnostics;
  - endpoint не раскрывает peer IP/ports.
- Frontend:
  - TypeScript build проходит после добавления BT health types/components.

## Definition of Done

- `clientConfig.Seed` включен по умолчанию, upload не отключен по умолчанию.
- qBittorrent-like profile включен по умолчанию и применен к `anacrolix/torrent.ClientConfig`.
- Public retrackers добавляются по умолчанию для magnet, bare infohash и `.torrent` add flow.
- DHT и PEX включены по умолчанию и видны в diagnostics.
- Connection acquisition параметры стали настраиваемыми через ENV.
- Завершенные torrent-ы остаются в engine и готовы к seeding.
- Web GUI показывает BT health diagnostics и warning про возможное отсутствие входящей connectivity без port-forward.
- Диагностика не раскрывает IP/ports конкретных peers.
- `docker-compose.yml` содержит новые ENV-настройки BT engine.
- Добавлены или обновлены релевантные тесты.
- Успешно проходят:
  - `go test ./...`
  - `(cd web && npm run build)`
  - `CGO_ENABLED=0 go build ./cmd/hub`
- Создан отчет `tasks/implementation/phase9-report.md`.

## Команды ручной проверки

```bash
curl -s http://localhost:8080/api/v1/health/bt | jq

curl -s http://localhost:8080/api/v1/torrents | jq '.[] | {hash, name, state, download_speed, upload_speed, peer_summary}'

docker compose logs -f torrent-hub
```

## Ручной сценарий проверки

1. Запустить сервис через Docker Compose.
2. Добавить public magnet через Web GUI или Lampa.
3. Проверить, что torrent получает trackers/retrackers и набирает peers.
4. Дождаться скачивания или использовать уже скачанный torrent.
5. Убедиться, что статус `Seeding` не означает остановку torrent-а.
6. Проверить BT health panel:
   - seed enabled;
   - upload enabled;
   - DHT/PEX enabled;
   - qBittorrent profile;
   - warning про incoming connectivity без port-forward.
7. Сравнить динамику peers/download speed с предыдущей версией сервиса на том же torrent-е.
