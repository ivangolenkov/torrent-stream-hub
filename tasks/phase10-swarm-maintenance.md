# Фаза 10: Swarm maintenance и восстановление скорости без port-forward

## Цель

Сделать BitTorrent engine устойчивым при длительной работе без ручного port-forward: сервис должен не только получать хороший initial burst peers после добавления torrent-а или рестарта, но и активно поддерживать swarm в рабочем состоянии, заменять деградировавшие соединения и восстанавливать скорость после падения.

Проблемы, которые должна закрыть фаза:

- После добавления torrent-а или рестарта скорость максимальная, затем постепенно падает.
- Количество connected peers/seeds со временем сокращается и не восстанавливается.
- При отсутствии port-forward сервис слишком пассивно живет на исходном наборе outgoing peers.
- BT Health показывает текущее состояние, но пока не объясняет, что engine считает torrent degraded и что было сделано для восстановления.
- Defaults peer acquisition недостаточно агрессивны для NAS/local streaming сценария.

## Принятые решения

1. Отсутствие port-forward не должно считаться фатальной ошибкой: сервис должен нормально работать в outgoing-only/degraded incoming mode.
2. Нужно добавить собственный swarm maintenance layer поверх `anacrolix/torrent`, а не полагаться только на initial announce и внутренний scheduler.
3. Peer refresh должен быть rate-limited, чтобы не устроить постоянный announce storm.
4. Диагностика должна оставаться aggregate-only: без IP/port конкретных peers.
5. Все изменения должны сохранять disk-backed модель и не вводить RAM cache.

## Задачи

### 1. Расширить конфигурацию swarm maintenance

Добавить ENV/flags в `internal/config/config.go`:

- `HUB_BT_SWARM_WATCHDOG_ENABLED`, default `true`.
- `HUB_BT_SWARM_CHECK_INTERVAL_SEC`, default `60`.
- `HUB_BT_SWARM_REFRESH_COOLDOWN_SEC`, default `180`.
- `HUB_BT_SWARM_MIN_CONNECTED_PEERS`, default `8`.
- `HUB_BT_SWARM_MIN_CONNECTED_SEEDS`, default `2`.
- `HUB_BT_SWARM_STALLED_SPEED_BPS`, default `32768`.
- `HUB_BT_SWARM_STALLED_DURATION_SEC`, default `180`.
- `HUB_BT_SWARM_BOOST_CONNS`, default `120`.
- `HUB_BT_SWARM_BOOST_DURATION_SEC`, default `300`.

Требования:

- Некорректные ENV значения должны fallback-иться к безопасным defaults.
- Значения должны логироваться на старте engine в sanitized виде.
- Если watchdog выключен, поведение engine должно остаться близким к фазе 9.

### 2. Поднять defaults peer acquisition

Обновить defaults в config и `docker-compose.yml`:

- `HUB_BT_ESTABLISHED_CONNS_PER_TORRENT`: с `50` до `120`.
- `HUB_BT_HALF_OPEN_CONNS_PER_TORRENT`: с `50` до `80`.
- `HUB_BT_TOTAL_HALF_OPEN_CONNS`: с `500` до `1000`.
- `HUB_BT_PEERS_LOW_WATER`: с `100` до `500`.
- `HUB_BT_PEERS_HIGH_WATER`: с `1000` до `1200`.
- `HUB_BT_DIAL_RATE_LIMIT`: с `20` до `60`.

Требования:

- Значения должны быть разумны для слабых NAS, но активнее текущих.
- Если пользователь задает собственные ENV, они должны иметь приоритет.
- Обновить пример конфигурации в `torrent-stream-hub-tz-v3.md`.

### 3. Добавить модель состояния swarm maintenance

В `internal/engine/engine.go` расширить `ManagedTorrent` runtime-only полями:

- `degraded bool`.
- `lastSwarmCheckAt time.Time`.
- `lastSwarmRefreshAt time.Time`.
- `lastSwarmRefreshReason string`.
- `lastHealthyAt time.Time`.
- `stallStartedAt time.Time`.
- `boostUntil time.Time`.
- `normalMaxEstablishedConns int`.

Требования:

- Эти поля не должны сохраняться в SQLite.
- При restore после restart поля инициализируются заново.
- Причины refresh должны быть sanitized и пригодны для UI/logs.

### 4. Реализовать degraded decision logic

Добавить отдельную тестируемую функцию или метод, который по aggregate stats решает, degraded ли torrent.

Учитывать:

- Torrent в состояниях `Downloading` или `Streaming` важнее, чем completed seeding torrent.
- Если metadata еще не готова и peers/seeds низкие дольше cooldown-а, это degraded.
- Если download speed ниже `HUB_BT_SWARM_STALLED_SPEED_BPS` дольше `HUB_BT_SWARM_STALLED_DURATION_SEC`, это degraded.
- Если connected peers ниже `HUB_BT_SWARM_MIN_CONNECTED_PEERS`, это degraded.
- Если connected seeds ниже `HUB_BT_SWARM_MIN_CONNECTED_SEEDS` для incomplete torrent-а, это degraded.
- Если torrent завершен и только seeding, низкая download speed не должна считаться degraded.

Требования:

- Решение должно быть покрыто unit-тестами без реальной сети.
- Избегать flapping: recovered только после явного улучшения stats или после периода без stall.

### 5. Реализовать SwarmRefreshMonitor

Добавить goroutine в `Engine.New()`:

- ticker с `HUB_BT_SWARM_CHECK_INTERVAL_SEC`.
- На каждом tick пройти по managed torrents.
- Для degraded torrent, если `refresh cooldown` истек, выполнить refresh actions.
- Для recovered torrent снять degraded state и вернуть boosted limits к normal.

Refresh actions:

- Повторно вызвать `DownloadAll()` для incomplete torrent с готовой metadata.
- Вызвать `AllowDataDownload()` и `AllowDataUpload()` если применимо.
- Временно поднять per-torrent max established conns через `SetMaxEstablishedConns(HUB_BT_SWARM_BOOST_CONNS)`.
- Запустить дополнительный DHT announce через доступные `e.client.DhtServers()` и `t.AnnounceToDht(server)`.
- Не делать tracker announce через private API `anacrolix/torrent`; если публичного API нет, полагаться на low-water/high-water и DHT refresh.

Требования:

- Refresh actions не должны держать engine lock во время сетевых операций.
- DHT announce должен выполняться async и не блокировать resource monitor.
- Cooldown обязателен.
- Если DHT выключен или servers отсутствуют, refresh должен логировать причину и выполнять остальные действия.

### 6. Улучшить интеграцию с resource manager

Проверить и при необходимости скорректировать `manageResourcesLocked()`:

- `StateSeeding` не должен выключать upload или connection acquisition.
- `StateDownloading` при готовой metadata должен гарантированно поддерживать `DownloadAll()` после refresh.
- Paused/Error/MissingFiles не должны участвовать в swarm refresh.
- DiskFull должен отключать download priorities, но после восстановления диска torrent должен снова участвовать в refresh.

Требования:

- Не нарушить `MaxActiveDownloads` для фоновой загрузки.
- Streaming QoS из `StreamManager` должен иметь приоритет над swarm boost.

### 7. Расширить BT Health diagnostics

Расширить `models.BTHealth` / `models.BTTorrentHealth`:

- global:
  - `swarm_watchdog_enabled`.
  - `swarm_check_interval_sec`.
  - `swarm_refresh_cooldown_sec`.
- per torrent:
  - `degraded`.
  - `last_refresh_at`.
  - `last_refresh_reason`.
  - `last_healthy_at`.
  - `boosted_until`.
  - `max_established_conns`.

Требования:

- Не раскрывать IP/ports peers.
- Поля времени отдавать в RFC3339 или пустой строкой/zero value, единообразно с существующим JSON style.
- UI должен показывать, когда engine пытался восстановить swarm.

### 8. Обновить Web GUI страницу BitTorrent Health

На странице `/health/bt` добавить отображение swarm maintenance:

- Показывать global status watchdog-а.
- Для каждого torrent показывать badge `Healthy` / `Degraded` / `Boosted`.
- Показывать `last_refresh_reason` и `last_refresh_at`.
- Показывать подсказку: без port-forward сервис может работать нормально, но зависит от активного outgoing peer refresh.

Требования:

- Сохранить текущий светлый UI стиль.
- Не возвращать BT Health panel на Dashboard.
- Mobile layout должен оставаться читаемым.

### 9. Логирование и observability

Добавить structured human-readable logs:

- когда torrent признан degraded;
- почему он degraded;
- какие refresh actions выполнены;
- когда torrent recovered;
- когда boost включен/выключен.

Требования:

- Логи не должны содержать peer IP/ports.
- Не логировать слишком часто: respect cooldown и debug/info уровни.

### 10. Тесты

Backend tests:

- Config tests для новых ENV/defaults.
- Unit tests для degraded decision logic:
  - низкие connected peers;
  - низкие connected seeds;
  - stalled speed;
  - completed seeding torrent не degraded из-за download speed;
  - cooldown предотвращает частые refresh.
- Engine tests для применения новых aggressive defaults в `ClientConfig`.
- API test: `GET /api/v1/health/bt` возвращает новые поля и не раскрывает peer IP/ports.

Frontend tests/build:

- TypeScript types обновлены.
- `(cd web && npm run build)` проходит.

## Definition of Done

- Добавлены ENV/flags swarm watchdog-а и aggressive peer acquisition defaults.
- `docker-compose.yml` обновлен новыми значениями.
- `torrent-stream-hub-tz-v3.md` обновлен примером Phase 10 config.
- Engine периодически проверяет swarm health и rate-limited refresh-ит degraded torrents.
- Refresh не блокирует engine lock и не создает announce storm.
- Incomplete torrents при деградации получают boost connections и дополнительный DHT announce, если DHT доступен.
- Completed seeding torrents не считаются stalled из-за отсутствия download speed.
- BT Health показывает degraded/boosted/last refresh diagnostics.
- Web GUI показывает новые поля на отдельной странице `/health/bt` в общем светлом стиле.
- Диагностика и логи не раскрывают IP/ports peers.
- Добавлены или обновлены релевантные тесты.
- Успешно проходят:
  - `go test ./...`
  - `(cd web && npm run build)`
  - `CGO_ENABLED=0 go build ./cmd/hub`
- Создан отчет `tasks/implementation/phase10-report.md`.

## Команды ручной проверки

```bash
curl -s http://localhost:8080/api/v1/health/bt | jq

curl -s http://localhost:8080/api/v1/torrents | jq '.[] | {hash, name, state, download_speed, upload_speed, peer_summary}'

docker compose logs -f torrent-hub
```

## Ручной сценарий проверки

1. Запустить сервис через Docker Compose без ручного port-forward на роутере.
2. Добавить public magnet с большим swarm.
3. Зафиксировать initial speed/peers/seeds в Web GUI и `/api/v1/health/bt`.
4. Оставить сервис работать минимум 30-60 минут.
5. Проверить, что при падении peers/speed torrent получает degraded status и refresh reason.
6. Проверить, что после refresh connected peers/seeds или download speed могут восстанавливаться без рестарта сервиса.
7. Проверить, что completed torrent остается seeding и не считается degraded из-за нулевой download speed.
8. Проверить, что BT Health не содержит IP/ports peers.
