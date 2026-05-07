# Phase 26: Report (Swarm Low-Speed Recovery)

## Что было сделано
- Добавлено runtime-отслеживание `peakDownloadSpeed` и `peakUpdatedAt` для активных `Downloading` торрентов.
- `checkSwarms()` теперь запускает soft refresh не только при нулевой скорости, но и при скорости ниже `BTSwarmStalledSpeedBps` или при падении ниже `peakDownloadSpeed * BTSwarmSpeedDropRatio`.
- Убраны hardcoded интервалы в low-speed watchdog: используются `BTSwarmStalledDurationSec` и `BTSwarmRefreshCooldownSec`.
- Сохранена защита pause-семантики: recovery logic применяется только к `models.StateDownloading`, поэтому paused/error/disk-full torrents не получают скрытый `DownloadAll()` через watchdog.
- В `models.BTTorrentHealth` и `Engine.BTHealth()` добавлены `peak_download_speed` и `peak_updated_at`.
- Добавлены unit-тесты для absolute low-speed, relative speed drop, paused safety, expired peak и config-based durations.

## Статус выполнения DoD
- [x] Watchdog восстанавливает torrent при устойчиво низкой ненулевой скорости.
- [x] Watchdog восстанавливает torrent при существенном падении скорости относительно актуального пика.
- [x] Watchdog больше не использует hardcoded `time.Minute` и `3*time.Minute`, если соответствующие config-поля уже есть.
- [x] Paused torrents не запускаются повторно через low-speed recovery.
- [x] `BT Health` показывает `peak_download_speed` и время обновления пика.
- [x] Низкая скорость не помечает завершенный seeding torrent как degraded/stalled.
- [x] Все backend-тесты проходят (`go test ./...`).
- [x] Отчет о реализации создан в `tasks/implementation/phase26-report.md` после фактической имплементации.

## Причины отхода от плана
- Hard refresh и client recycle не затрагивались: фаза ограничена low-speed soft refresh.
- UI-дизайн не менялся: backend восстановил уже ожидаемые фронтендом поля `peak_download_speed` и `peak_updated_at`.
