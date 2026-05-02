# Phase 14 Report: Download speed parity

## Что было сделано

- Добавлен `HUB_BT_DOWNLOAD_PROFILE` с профилями:
  - `torrserver`;
  - `balanced`;
  - `aggressive`.
- Connection defaults стали profile-driven:
  - explicit ENV/flags продолжают переопределять profile defaults;
  - `balanced` теперь использует более умеренные half-open/peer watermarks для домашнего NAT.
- Добавлен `HUB_BT_BENCHMARK_MODE`:
  - automatic swarm recovery mutations suppressed;
  - diagnostics остаются видимыми;
  - manual actions остаются доступны.
- Добавлены Public IP настройки:
  - `HUB_BT_PUBLIC_IP_DISCOVERY_ENABLED`;
  - `HUB_BT_PUBLIC_IPV4`;
  - `HUB_BT_PUBLIC_IPV6`;
  - invalid/private IP rejected;
  - discovery best-effort с коротким timeout, startup не падает.
- Добавлен persistent metainfo storage:
  - metainfo сохраняется в `/config/metainfo/<hash>.torrent`;
  - `.torrent` upload сохраняет исходный metainfo;
  - magnet после metadata ready сохраняет generated metainfo;
  - restore priority: metainfo file -> persisted magnet -> infohash fallback.
- Расширен `/health/bt` aggregate diagnostics:
  - raw/data/useful byte counters;
  - raw/data/useful download speed;
  - chunk useful/wasted counters;
  - waste ratio;
  - tracker tiers/URLs count;
  - active download profile/effective limits;
  - benchmark mode;
  - public IP status.
- Обновлен UI `/health/bt`:
  - показывает active download profile и effective limits;
  - показывает public IP status;
  - показывает benchmark mode banner;
  - показывает raw/data/useful speed и waste ratio per torrent;
  - показывает tracker tier/url counts.
- Обновлены `docker-compose.yml` и `torrent-stream-hub-tz-v3.md`.
- После runtime анализа исправлен `RecycleClient`:
  - старый BT client теперь закрывается до создания нового;
  - rebind нового клиента выполняется с retry, чтобы listen port успел освободиться;
  - это устраняет observed error `listen tcp4 :50007: bind: address already in use`.
- Из default retrackers удален `wss://tracker.btorrent.xyz`, который стабильно давал TLS certificate error и засорял диагностику.
- Добавлены/обновлены tests:
  - config profile defaults;
  - explicit override behavior;
  - private/public IP validation;
  - engine config defaults.

## Статус DoD

- [x] BT Health exposes enough aggregate counters to compare raw/data/useful speed.
- [x] Benchmark mode can suppress automatic recovery mutations.
- [x] Download profiles exist and are visible in diagnostics.
- [x] Effective connection limits are profile-driven but ENV-overridable.
- [x] Public IP config/discovery is supported without breaking startup.
- [x] Metainfo is persisted and used before magnet/infohash on restore.
- [x] Tracker count diagnostics exist and do not expose peer IP/ports.
- [x] Upload disabled warning remains covered by existing upload diagnostics; no forced upload policy change.
- [x] `/health/bt` can be used for side-by-side qBittorrent/TorrServer comparison.
- [x] `docker-compose.yml` and `torrent-stream-hub-tz-v3.md` updated.
- [x] Created report `tasks/implementation/phase14-report.md`.
- [x] Successful checks:
  - [x] `go test ./...`
  - [x] `(cd web && npm run build)`
  - [x] `CGO_ENABLED=0 go build ./cmd/hub`

## Причины отхода от плана

- Public IP discovery реализован через best-effort HTTP endpoint с коротким timeout вместо добавления новой зависимости. Это снижает риск dependency churn и не влияет на startup при disabled default.
- Upload warning не выделен отдельным новым UI баннером, потому что global `upload_enabled`, `upload_limit` и per-torrent upload speed уже видимы; можно усилить текст после первого benchmark, если upload окажется фактором.
- Full benchmark automation не добавлялась: в этой фазе добавлены diagnostics и режимы, а фактическое сравнение требует одинаковых внешних условий swarm/router/disk.
