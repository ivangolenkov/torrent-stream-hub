# Phase 26: Swarm Low-Speed Recovery

## Описание
Улучшить механизм восстановления BitTorrent-загрузок, чтобы он реагировал не только на полностью нулевую скорость, но и на устойчиво низкую скорость скачивания.

Сейчас `checkSwarms()` запускает soft refresh только если `downloadSpeed == 0` держится около минуты. На практике торрент может продолжать качать данные, но сильно деградировать: например, скорость падает с нескольких MB/s до десятков KB/s. Такое состояние не считается stalled, хотя для пользователя загрузка фактически сломана или идет неприемлемо медленно.

Механизм восстановления должен учитывать два вида деградации:
- абсолютную: скорость ниже `BTSwarmStalledSpeedBps`;
- относительную: скорость сильно упала относительно недавнего пика (`BTSwarmSpeedDropRatio`).

Важно сохранить исправление паузы: paused/queued/error/disk-full torrents не должны попадать в recovery logic и не должны получать `DownloadAll()` через watchdog.

## Задачи

### 1. Peak Speed Tracking
- [ ] Добавить runtime-поля в `ManagedTorrent` для отслеживания пика полезной скорости скачивания: `peakDownloadSpeed int64` и `peakUpdatedAt time.Time`.
- [ ] Обновлять `peakDownloadSpeed` в `updateSpeedsLocked()` только для положительной `downloadSpeed`.
- [ ] Сбрасывать или обновлять peak после истечения `BTSwarmPeakTTLSec`, чтобы старый пик не делал торрент "вечнo degraded".
- [ ] Не учитывать peak для завершенных (`Seeding`) и неактивных (`Paused`, `Error`, `DiskFull`, `MissingFiles`) торрентов.

### 2. Low-Speed Decision Logic
- [ ] Вынести определение необходимости refresh в небольшую тестируемую функцию или аккуратно локализованный блок внутри `checkSwarms()`.
- [ ] Считать torrent кандидатом на recovery только если он в состоянии `models.StateDownloading`, metadata готова, torrent не завершен и нет активного pause/error/disk-full состояния.
- [ ] Считать скорость деградировавшей, если `downloadSpeed < BTSwarmStalledSpeedBps`.
- [ ] Считать скорость деградировавшей, если `peakDownloadSpeed` актуален и `downloadSpeed < peakDownloadSpeed * BTSwarmSpeedDropRatio`.
- [ ] Не запускать refresh мгновенно: low-speed состояние должно сохраняться не меньше `BTSwarmStalledDurationSec`.
- [ ] Использовать `BTSwarmRefreshCooldownSec` для rate-limit soft refresh, а не hardcoded `3*time.Minute`.
- [ ] В `lastSwarmRefreshReason` писать различимые причины: например `stalled_speed_below_threshold` и `speed_dropped_below_peak`.

### 3. Safety With Pause Semantics
- [ ] Убедиться, что `checkSwarms()` сбрасывает `stallStartedAt` для всех состояний, кроме `Downloading`.
- [ ] Убедиться, что low-speed recovery не вызывает `refreshPeerDiscovery()` для paused torrents.
- [ ] Сохранить поведение `Pause()`: `SetPriority(None)`, `CancelPieces()`, `DisallowDataDownload()` и запрет скрытого restart через watchdog.
- [ ] Сохранить поведение `Resume()`: разрешение скачивания возвращается только через явный `Resume()` и дальнейший `manageResources()`.

### 4. BT Health Diagnostics
- [ ] Добавить `PeakDownloadSpeed int64` и `PeakUpdatedAt string` в `models.BTTorrentHealth`.
- [ ] Прокинуть эти поля из `ManagedTorrent` в `Engine.BTHealth()`.
- [ ] Убедиться, что существующий фронтенд `BTHealthPanel.vue` получает `peak_download_speed` и `peak_updated_at` с backend, а не показывает нули.
- [ ] При необходимости синхронизировать TypeScript типы с backend-моделью, не ломая существующий UI.

### 5. Tests
- [ ] Добавить unit-тест или engine-тест: нулевая скорость у `Downloading` torrent после configured duration вызывает soft refresh decision.
- [ ] Добавить тест: скорость ниже `BTSwarmStalledSpeedBps`, но выше нуля, вызывает refresh после duration.
- [ ] Добавить тест: скорость ниже `peakDownloadSpeed * BTSwarmSpeedDropRatio` вызывает refresh после duration.
- [ ] Добавить тест: paused torrent с нулевой или низкой скоростью не вызывает refresh и не меняет priorities/download permission.
- [ ] Добавить тест: старый peak после `BTSwarmPeakTTLSec` не вызывает relative-speed degraded decision.
- [ ] Проверить backend командой `go test ./...`.

## Definition of Done (DoD)
- [ ] Watchdog восстанавливает torrent при устойчиво низкой ненулевой скорости.
- [ ] Watchdog восстанавливает torrent при существенном падении скорости относительно актуального пика.
- [ ] Watchdog больше не использует hardcoded `time.Minute` и `3*time.Minute`, если соответствующие config-поля уже есть.
- [ ] Paused torrents не запускаются повторно через low-speed recovery.
- [ ] `BT Health` показывает `peak_download_speed` и время обновления пика.
- [ ] Низкая скорость не помечает завершенный seeding torrent как degraded/stalled.
- [ ] Все backend-тесты проходят (`go test ./...`).
- [ ] Отчет о реализации создан в `tasks/implementation/phase26-report.md` после фактической имплементации.

## Не входит в фазу
- [ ] Не реализовывать hard refresh или client recycle заново, если они не требуются для low-speed soft refresh.
- [ ] Не менять UI-дизайн BT Health, кроме восстановления уже ожидаемых диагностических полей.
- [ ] Не менять семантику pause/resume за пределами защиты от скрытого restart.
