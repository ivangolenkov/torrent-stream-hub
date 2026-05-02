# Фаза 14: Download speed parity с qBittorrent/TorrServer

## Цель

Добиться максимально близкой скорости обычного скачивания торрентов по сравнению с qBittorrent и TorrServer на одинаковых раздачах.

Эта фаза не про HTTP-стриминг, seek/readahead и recovery. Приоритет — базовая BitTorrent download performance:

- быстрее находить и подключать peers/seeds;
- стабильнее держать полезную скорость загрузки;
- не перегружать NAT/router/NAS чрезмерным dial/half-open pressure;
- сохранять richer torrent source между рестартами;
- получить диагностику, которая объясняет, где именно теряется скорость.

## Контекст

По сравнению с TorrServer у нас уже есть часть важных настроек:

- qBittorrent-like identity:
  - `HTTPUserAgent = qBittorrent/4.3.9`;
  - `ExtendedHandshakeClientVersion = qBittorrent/4.3.9`;
  - `Bep20 = -qB4390-`;
  - randomized PeerID;
- DHT/PEX/TCP/uTP/UPnP включены по умолчанию;
- public retrackers добавляются к `TorrentSpec`;
- upload/seeding включены по умолчанию.

Но есть важные отличия:

- TorrServer пытается определить и выставить `PublicIp4/PublicIp6`;
- TorrServer использует более умеренный `TotalHalfOpenConns = 500`;
- у нас текущий default может быть слишком агрессивен для домашнего NAT:
  - `EstablishedConnsPerTorrent=120`;
  - `HalfOpenConnsPerTorrent=80`;
  - `TotalHalfOpenConns=1000`;
  - `PeersLowWater=500`;
  - `PeersHighWater=1200`;
  - `DialRateLimit=60`;
- `.torrent` upload сейчас сохраняется как magnet source, из-за чего при restore может теряться tracker richness/metainfo;
- текущая диагностика скорости показывает не все raw/useful counters, поэтому сложно отличить реальную медленную загрузку от отличий в подсчете скорости.

## Основной принцип фазы

Сначала измерить и выровнять обычную загрузку. Не добавлять новые recovery-механизмы и не переписывать streaming storage в этой фазе.

Во время benchmark:

- один активный torrent;
- без HTTP stream;
- watchdog/recycle не должен вмешиваться в первые измерения;
- сравнение делать с тем же magnet/.torrent, тем же портом, тем же диском и тем же роутером.

## Задачи

### 1. Расширить BT download diagnostics

Расширить `models.BTTorrentHealth` aggregate-only полями из `torrent.TorrentStats`:

- `bytes_read`;
- `bytes_read_data`;
- `bytes_read_useful_data`;
- `bytes_written`;
- `bytes_written_data`;
- `chunks_read`;
- `chunks_read_useful`;
- `chunks_read_wasted`;
- `pieces_dirtied_good`;
- `pieces_dirtied_bad`;
- `total_peers` alias или использовать existing `known`;
- `active_peers` alias или использовать existing `connected`;
- `pending_peers` alias или использовать existing `pending`;
- `half_open_peers` alias или использовать existing `half_open`;
- `connected_seeders` alias или использовать existing `seeds`.

Добавить derived diagnostics:

- `raw_download_speed` на основе `BytesRead`;
- `data_download_speed` на основе `BytesReadData`;
- `useful_download_speed` на основе `BytesReadUsefulData`;
- `waste_ratio` если можно безопасно вычислить.

Требования:

- Не раскрывать peer IP/ports.
- Не добавлять per-peer diagnostics.
- Не хранить эти counters в SQLite.
- UI `/health/bt` должен показывать compact view, без перегруза Dashboard.

### 2. Добавить download benchmark mode

Добавить config:

- `HUB_BT_BENCHMARK_MODE`, default `false`.

Когда включен:

- swarm watchdog может оставаться видимым в diagnostics, но destructive recovery actions не запускаются автоматически;
- automatic client recycle disabled for benchmark;
- automatic hard refresh disabled;
- soft refresh может быть либо disabled, либо только logged. Предпочтительно: no mutation, only diagnostics.

Требования:

- Manual actions в UI остаются доступны.
- BT Health показывает `benchmark_mode=true`.
- Логи явно пишут, что recovery suppressed by benchmark mode.

### 3. Ввести download performance profiles

Расширить `HUB_BT_CLIENT_PROFILE` или добавить отдельный config `HUB_BT_DOWNLOAD_PROFILE`.

Рекомендуемый вариант: новый `HUB_BT_DOWNLOAD_PROFILE`, default `balanced`.

Profiles:

#### `torrserver`

Цель: приблизиться к сетевой конфигурации TorrServer.

- qBittorrent-like identity сохраняется;
- `TotalHalfOpenConns=500` default;
- `HalfOpenConnsPerTorrent=40` default;
- `EstablishedConnsPerTorrent=100` default;
- `DialRateLimit=40` default;
- `PeersLowWater=300` default;
- `PeersHighWater=800` default;
- `UpnpID = Torrent-Stream-Hub` or `TorrServer-compatible`.

#### `balanced`

Цель: стабильная скорость на домашнем роутере/NAS.

- `EstablishedConnsPerTorrent=120`;
- `HalfOpenConnsPerTorrent=60`;
- `TotalHalfOpenConns=700`;
- `DialRateLimit=60`;
- `PeersLowWater=400`;
- `PeersHighWater=1000`.

#### `aggressive`

Цель: максимум для сильного устройства/роутера.

- `EstablishedConnsPerTorrent=200`;
- `HalfOpenConnsPerTorrent=100`;
- `TotalHalfOpenConns=1200`;
- `DialRateLimit=120`;
- `PeersLowWater=700`;
- `PeersHighWater=1600`.

Требования:

- Явные ENV значения должны переопределять profile defaults.
- Config tests должны проверять profile defaults и override behavior.
- BT Health показывает active download profile и effective limits.

### 4. Добавить Public IP discovery/config

Добавить config:

- `HUB_BT_PUBLIC_IP_DISCOVERY_ENABLED`, default `false` для предсказуемого старта;
- `HUB_BT_PUBLIC_IPV4`, optional;
- `HUB_BT_PUBLIC_IPV6`, optional.

Поведение:

1. Если `HUB_BT_PUBLIC_IPV4` задан и валиден/public, выставить `clientConfig.PublicIp4`.
2. Если `HUB_BT_PUBLIC_IPV6` задан и валиден/public, выставить `clientConfig.PublicIp6`.
3. Если discovery enabled, попытаться определить public IP через публичный API/библиотеку с коротким timeout.
4. Если discovery failed, старт сервиса не должен падать.

Требования:

- Не использовать private/local IP как public IP.
- Не логировать лишние network details.
- BT Health показывает:
  - `public_ip_discovery_enabled`;
  - `public_ipv4_status`: `configured|discovered|disabled|failed|invalid`;
  - `public_ipv6_status`: `configured|discovered|disabled|failed|invalid`.

### 5. Сохранение metainfo для restore

Решить потерю tracker richness после рестарта.

Добавить persistent metainfo storage:

Вариант A, предпочтительный:

- сохранять `.torrent` bytes в `/config/metainfo/<hash>.torrent`;
- путь не хранить в SQLite, вычислять по hash.

Вариант B:

- добавить SQLite table/blob для metainfo bytes.

Требования:

- При `AddTorrentFile` сохранять исходный metainfo.
- Когда magnet torrent получает metadata, сохранять generated `t.Metainfo()`.
- При restore использовать priority:
  1. `/config/metainfo/<hash>.torrent`;
  2. persisted `source_uri` magnet;
  3. bare infohash fallback.
- Re-add source должен отображаться в BT Health:
  - `metainfo_file`;
  - `metainfo_runtime`;
  - `magnet`;
  - `infohash`.
- Не ломать существующую SQLite migration chain.

### 6. Проверить tracker/retracker parity

Добавить diagnostics:

- `tracker_tiers_count`;
- `tracker_urls_count`;
- `retrackers_mode` уже есть global;
- `last_tracker_status`/`last_tracker_error` уже есть per torrent.

Проверить кодом/тестами:

- magnet получает retrackers;
- `.torrent` сохраняет original trackers + retrackers in append mode;
- restore из metainfo сохраняет trackers;
- restore из magnet fallback явно показывает это в health.

### 7. Upload policy diagnostics

Добавить в BT Health:

- `seed_enabled` already exists;
- `upload_enabled` already exists;
- `upload_limit` already exists;
- добавить per torrent `upload_speed` уже есть;
- добавить warning, если upload disabled: это может ухудшать download в некоторых swarms.

Требования:

- Ничего не форсировать, только diagnostics/recommendation.
- Default upload remains enabled.

### 8. UI для benchmark comparison

Обновить `/health/bt`:

- показывать active download profile;
- показывать effective connection limits;
- показывать raw/data/useful speed рядом;
- показывать waste ratio;
- показывать tracker count;
- показывать public IP status;
- показывать benchmark mode banner.

Требования:

- Страница остается readable на mobile.
- Dashboard не перегружать.

### 9. Docker Compose и ТЗ

Обновить `docker-compose.yml`:

- добавить `HUB_BT_DOWNLOAD_PROFILE=balanced`;
- добавить optional commented/public IP discovery settings if compose style allows;
- добавить `HUB_BT_BENCHMARK_MODE=false`.

Обновить `torrent-stream-hub-tz-v3.md`:

- описать download profiles;
- описать benchmark mode;
- описать metainfo persistence;
- добавить benchmark procedure.

### 10. Тесты

Backend:

- config profile defaults;
- env override beats profile defaults;
- public IP validation rejects private IP;
- metainfo file save/load path;
- restore priority: metainfo file -> magnet -> infohash;
- retracker append preserved for `.torrent` and restore;
- benchmark mode suppresses automatic recovery mutation;
- BT Health returns new diagnostics without peer IP/ports.

Frontend:

- TypeScript types updated;
- `(cd web && npm run build)` passes.

## Definition of Done

- BT Health exposes enough aggregate counters to compare raw/data/useful speed.
- Benchmark mode can suppress automatic recovery mutations.
- Download profiles exist and are visible in diagnostics.
- Effective connection limits are profile-driven but ENV-overridable.
- Public IP config/discovery is supported without breaking startup.
- Metainfo is persisted and used before magnet/infohash on restore.
- Tracker count diagnostics exist and do not expose peer IP/ports.
- Upload disabled warning exists.
- `/health/bt` can be used for side-by-side qBittorrent/TorrServer comparison.
- `docker-compose.yml` and `torrent-stream-hub-tz-v3.md` updated.
- Created report `tasks/implementation/phase14-report.md`.
- Successful checks:
  - `go test ./...`
  - `(cd web && npm run build)`
  - `CGO_ENABLED=0 go build ./cmd/hub`

## Ручной benchmark сценарий

1. Остановить все активные загрузки/стримы.
2. Включить benchmark mode:
   - `HUB_BT_BENCHMARK_MODE=true`.
3. Выбрать profile:
   - сначала `HUB_BT_DOWNLOAD_PROFILE=torrserver`;
   - затем `balanced`;
   - затем `aggressive`.
4. Запустить один и тот же public torrent в Torrent-Stream-Hub, qBittorrent и TorrServer по очереди.
5. Для каждого запуска фиксировать 10 минут после metadata ready:
   - peak useful speed;
   - average useful speed;
   - raw/data/useful speed difference;
   - connected peers;
   - connected seeders;
   - pending peers;
   - half-open peers;
   - tracker status;
   - waste ratio.
6. Не менять роутер/порт/disk между запусками.
7. Если `aggressive` хуже `balanced`, считать это признаком NAT/router saturation.
8. Если peers/seeds значительно ниже qBittorrent, фокус на peer acquisition/tracker/DHT/PublicIP.
9. Если peers/seeds похожи, но useful speed ниже, фокус на choking/upload/storage/backpressure.
