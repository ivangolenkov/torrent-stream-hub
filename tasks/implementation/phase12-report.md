# Отчет по фазе 12: Reliable refresh и client recycle fallback

## Что было сделано

- Добавлены настройки escalation/client recycle в `internal/config/config.go`:
  - `HUB_BT_SWARM_DEGRADATION_EPISODE_TTL_SEC`;
  - `HUB_BT_SWARM_RECOVERY_GRACE_SEC`;
  - default `HUB_BT_SWARM_HARD_REFRESH_AFTER_SOFT_FAILS=1`;
  - default `HUB_BT_SWARM_HARD_REFRESH_MIN_TORRENT_AGE_SEC=60`;
  - `HUB_BT_CLIENT_RECYCLE_ENABLED`;
  - `HUB_BT_CLIENT_RECYCLE_COOLDOWN_SEC`;
  - `HUB_BT_CLIENT_RECYCLE_AFTER_HARD_FAILS`;
  - `HUB_BT_CLIENT_RECYCLE_MIN_TORRENTS`;
  - `HUB_BT_CLIENT_RECYCLE_MAX_PER_HOUR`.
- Добавлен degradation episode state в `ManagedTorrent`:
  - episode start;
  - last degraded/recovered;
  - last soft refresh;
  - soft/hard attempts within episode;
  - next hard refresh/client recycle timestamps.
- Изменена escalation logic:
  - краткое recovery больше не сбрасывает soft refresh eligibility;
  - reset episode выполняется только после stable recovery grace;
  - hard refresh использует episode counters вместо простого flapping state.
- Добавлен manual hard refresh:
  - `POST /api/v1/torrent/{hash}/action` с `action: "hard_refresh"`;
  - usecase wrapper;
  - engine exported `HardRefresh`.
- Добавлен in-process client recycle fallback:
  - пересоздание `anacrolix/torrent.Client` без рестарта HTTP server-а;
  - re-add managed torrents из `sourceURI` или fallback infohash;
  - сохранение runtime state/source/diagnostics;
  - active stream/cooldown/hourly limit gates.
- Добавлен manual recycle endpoint:
  - `POST /api/v1/health/bt/recycle`.
- Расширен `BTHealth`:
  - client recycle status/counters/reason/error;
  - degradation episode diagnostics;
  - `next_hard_refresh_at` и `next_client_recycle_at`.
- Обновлен Web GUI `/health/bt`:
  - кнопка `Hard refresh` per torrent;
  - кнопка `Recycle client`;
  - episode soft/hard attempts;
  - next hard refresh time;
  - client recycle counters/block reason.
- Обновлены `docker-compose.yml` и `torrent-stream-hub-tz-v3.md`.

## Статус выполнения DoD

- [x] Hard refresh запускается в flapping degraded swarm-е и не теряет eligibility из-за краткого recovery.
- [x] Manual hard refresh доступен через Web API и `/health/bt` UI.
- [x] Client recycle реализован как in-process fallback без рестарта HTTP server-а.
- [x] Client recycle пересоздает `anacrolix/torrent.Client`, re-adds torrents и сохраняет files/SQLite metadata/source state.
- [x] Active streams блокируют hard refresh и client recycle.
- [x] BT Health показывает точную причину блокировки и `next_*_at`.
- [x] Web GUI показывает hard refresh/client recycle controls и diagnostics.
- [x] Diagnostics/logs не раскрывают peer IP/ports.
- [x] `docker-compose.yml` и `torrent-stream-hub-tz-v3.md` обновлены.
- [x] Создан отчет `tasks/implementation/phase12-report.md`.
- [x] `go test ./...`.
- [x] `(cd web && npm run build)`.
- [x] `CGO_ENABLED=0 go build ./cmd/hub`.

## Причины отхода от плана

- Manual hard refresh не bypass-ит min age/cooldown. Это оставлено как safety constraint, чтобы UI action не мог вызвать частый runtime churn.
- Partial failure при client recycle записывается в global `last_client_recycle_error`; torrents, которые не удалось re-add, не получают полноценный placeholder runtime object, потому что текущая engine map привязана к `*torrent.Torrent`. Это стоит улучшить отдельной задачей, если partial failures начнут встречаться на практике.
