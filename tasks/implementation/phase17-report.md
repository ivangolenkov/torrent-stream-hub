# Фаза 17: Performance-driven peer refresh — отчет

## Что было сделано

- Настроен Boost limit:
  - `HUB_BT_SWARM_BOOST_CONNS` теперь по умолчанию принимает значение `BTEstablishedConns * 2`, если оно меньше или равно ему, делая boost осмысленным.
- Разделен Swarm Decision:
  - `decideSwarmHealth` переписан, чтобы возвращать отдельное поле `NeedsRefresh` (для performance refresh) помимо `Degraded` (для UI warnings/errors).
  - Теперь даже при активной загрузке (>32 KB/s) могут срабатывать трендовые правила `connected peers dropped below recent peak`, `connected seeds dropped...`, и `download speed dropped...` с установкой `NeedsRefresh = true`.
  - Добавлено правило истощения Peer Pool: если `Pending == 0 && HalfOpen == 0 && Connected < cfg.BTEstablishedConns && Known <= Connected+2`, генерируется refresh.
- Интеграция Engine:
  - `checkSwarms()` вызывает non-destructive peer refresh, когда `decision.NeedsRefresh = true`.
  - Обновлена отправка полей `Known`, `Pending`, и `HalfOpen` в `swarmSnapshot`.
- Тесты обновлены:
  - `swarm_test.go` покрывает новые правила `NeedsRefresh`.

## Статус выполнения DoD

- [x] Снижение скорости от пика вызывает refresh.
- [x] Истощение пула кандидатов вызывает refresh.
- [x] Рабочие загрузки со скоростью > 32 KB/s могут вызывать refresh без статуса "degraded".
- [x] Boost conns автоматически превышает established conns.
- [x] Тесты проходят успешно.
