# Фаза 17: Performance-driven peer refresh

## Цель

Текущая реализация (Phase 16) успешно предотвращает destructive recovery, но слишком консервативна в запуске non-destructive peer refresh. Если скорость падает с 12 MB/s до 60 KB/s (но остается выше stalled threshold 32 KB/s), refresh не запускается, и загрузка "ползет".
Также `HUB_BT_SWARM_BOOST_CONNS` по умолчанию равен `established_conns`, что делает boost бесполезным.

Фаза должна:
1. Разделить "деградацию" (для UI) и "потребность в refresh" (для performance).
2. Запускать peer refresh при падении скорости от пика, даже если скорость > 32 KB/s.
3. Запускать peer refresh при истощении peer pool (pending=0, half_open=0, known ~ connected).
4. Сделать boost лимит соединений осмысленным (по умолчанию x2 от established).

## Задачи

### 1. Настройка Boost соединений
Изменить `config.go`:
- Если `BTSwarmBoostConns <= BTEstablishedConns`, установить его в `BTEstablishedConns * 2` (или +80, если x2 слишком много, например, limit max).
- Обновить тесты конфигурации.

### 2. Разделение Swarm Decision
Изменить `decideSwarmHealth` (или заменить на `decideSwarmAction`):
Возвращать структуру:
```go
type swarmDecision struct {
    Degraded bool
    NeedsRefresh bool
    Reason string
}
```
Логика:
- Если stalled, low peers (когда не качает) - `Degraded = true`, `NeedsRefresh = true`.
- Если active download (скорость > stalled), но есть trend drop (speed, peers, seeds) - `Degraded = false`, `NeedsRefresh = true`.
- Если peer pool empty (`pending == 0 && half_open == 0 && known <= connected + 2` при неполном коннекте) - `Degraded = false`, `NeedsRefresh = true`.

### 3. Интеграция в Engine
Изменить `checkSwarms()`:
- `mt.degraded = decision.Degraded`.
- Trigger `refreshPeerDiscovery` если `decision.NeedsRefresh` (с учетом cooldown).

### 4. Тесты
- Обновить `swarm_test.go` под новую логику.

## Definition of Done
- Снижение скорости от пика вызывает refresh.
- Истощение пула кандидатов вызывает refresh.
- Рабочие загрузки со скоростью > 32 KB/s могут вызывать refresh без статуса "degraded".
- Boost conns автоматически превышает established conns.
- Тесты проходят успешно.
