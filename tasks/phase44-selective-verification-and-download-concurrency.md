# Фаза 44: Избирательная проверка данных без помех скачиванию

## Статус и зависимости

- Статус: **основной runtime реализован, локальный `make check` пройден**. Negative startup planning, signature/write publication, received-piece priority и cached диагностика проверены локально. Отдельные measurements и native Windows gate остаются открыты; подробности в [отчёте](implementation/phase44-report.md). Фаза не объявляется завершённой.
- Зависимость: фаза 43 завершена; сохраняются [контракт fast resume](phase43-fast-resume-contract.md), гарантии хранения фаз 41–42 и действующий lifecycle/coordinator.
- Этот файл — план, не команда на изменение runtime или развёрнутого сервиса. Начинать реализацию по отдельной команде пользователя; блоки выполнять последовательно после проверки приёмки предыдущего.
- После каждого значимого блока создавать/обновлять `tasks/implementation/phase44-report.md`: изменения, команды, измерения, DoD и отклонения. Следующую фазу автоматически не начинать.

### Follow-up: просадки скорости после первого исправления

27 октября детерминированным sync barrier воспроизведено удержание file-wide
payload lease через completion fsync, блокирующее соседних network writers.
В VerifyOnOpen Flush вынесен перед короткой publication lease, затем исходный
hashSnapshot повторно проверяется под lease; lifecycle/revocation и обязательный
sync сохраняются. Добавлены cached `BT Health → storage_io` timings и тесты
мутаций во время/после sync. Измерения, проверки и ограничения — в конце отчёта.
Улучшение на пользовательском NAS пока не подтверждено: деплой не выполнялся.

## Проблема и исходные наблюдения

При read-only диагностике `http://192.168.31.126:8080/` в логах сервиса от **2026-10-26 23:07–23:10** обнаружено:

- Новый magnet `a7af700105f73eaf1d8f3142d8c5074f40729f4e`, 10 файлов, 15 166 761 806 bytes, 7233 pieces по 2 MiB; `resume.mode=fast`, fallback `no_checkpoint`.
- Начальная проверка перебирает pieces ещё не созданных файлов и пишет тысячи `finished hashing piece ... correct=false ... no such file or directory`. API классифицирует такие файлы как `missing / not_created`.
- `error marking piece complete ... storage changed during verification` для pieces 18, 3310, 37.
- Первый запрос реального стрима завершился после ~12 s с `written=0`, следующий — после ~13 s с `written=2097152`; оба отменены клиентом. Одновременно есть сиды и ненулевая скорость скачивания, следовательно полного запрета download нет.
- Проверка продвигалась: очередь уменьшалась с 1387 до 869 pieces. Нельзя объявлять её зависшей или доказывать причинность падения скорости только этими логами.

Кандидат причины конфликтов — согласование file-wide signatures и параллельных внутренних writes: `storage/file-piece.go` снимает текущий stat до `freshMu`, а `acceptFileWrite` обновляет общую подпись файла. Это **гипотеза для воспроизведения**, не установленная причина всех ошибок. Повторное чтение частично заполненного файла также может быть дорогим; учитывать влияние inventory, polling, SQL, flush и планировщика, а не только hashing.

## Цель

Новый торрент без существующего payload начинает скачивание без бессмысленной проверки отсутствующих диапазонов. Существующие непроверенные данные проходят проверку перед использованием; пригодный checkpoint восстанавливает свой subset без повторного hashing. Нормальная запись независимых pieces не аннулирует их успешную проверку и не вызывает бесконечный rehash.

**Не отключать проверку хешей полученных от пиров частей.** Ускорение достигается исключением ненужной работы и правильной синхронизацией, а не выдачей непроверенных bytes за исправные.

## Область и ограничения

- Использовать существующий fast resume, opaque restore tokens, hash budgets, storage ownership, attachments и durable revocations; не создавать вторую систему доверия или блокировок.
- Старый `piece_completion`, размер, имя файла, sparse allocation или успешный stat не дают positive completion/readiness. Отрицательное evidence может лишь исключить заведомо отсутствующий диапазон из проверки.
- `fast` остаётся default; `verify` и explicit recheck сохраняют проверку существующего содержимого. Пропуск достоверно отсутствующего диапазона допустим только как **incomplete**, не как успешно проверенное содержимое.
- Сохранить SQLite WAL/FULL и пул, revoke-before-write, quiesce/flush/seal, rooted containment, aliases, journal/volume gates, Pause, selection, temporary policy, sticky DiskFull, FIFO, CORS `*`, Range и QoS/refcount/debounce. Не включать mmap.
- Не создавать, truncate или чинить payload ради классификации/проверки. `not_created` нового download не равен потере ранее существовавших файлов. MissingFiles/access denied/volume mismatch и recovery workflow не превращаются в автоматический repair или Resume.
- Не обещать filesystem watcher, scrub и обнаружение скрытой same-size/same-mtime порчи в `fast`: ограничения фазы 43 сохраняются.
- Диагностика на пользовательском сервере — только GET по умолчанию. Restart, recheck, deploy, изменение конфигурации и данных требуют отдельного разрешения.

## Блок 1 — Контракт, воспроизведение и baseline

- Проследить open/restore/fallback, `BeginVerification → hash → MarkComplete`, внутренние writes, refresh/revoke signatures, повторную постановку pieces и stream scheduling в fork и engine. Отдельно проверить promotion/import `.part`, v2 layers и periodic seal.
- Детерминированными barriers воспроизвести изменение соседнего piece того же файла между stat и публикацией результата; отдельно — запись в проверяемый диапазон, внешний replace/truncate и stale callback. Не подменять воспроизведение случайным `sleep` или одним успешным race-прогоном.
- Зафиксировать в отчёте правила классификации и invalidation, порядок locks/I/O, единицу mutation generation и критерий допустимости hash result. Если нужен новый runtime token, описать его scope/lifetime и fencing до реализации.
- Baseline на изолированных fixtures: пустой новый торрент с геометрией 7233 pieces/10 файлов, частичный файл, полные существующие данные без checkpoint, валидный partial checkpoint, повреждённый checkpoint. Реальные fixtures могут быть меньше по payload; отдельно указать synthetic geometry и фактически выделенные bytes.
- Измерять payload hash bytes, hash attempts/negative skips, stat/open, SQL transactions, retries/revocations, queue wait, time to first verified stream Range, время проверки, download throughput и peak working memory. Сеть заменить локальным управляемым seeder; не использовать публичный swarm как воспроизводимый benchmark.

**Приёмка:** конфликт воспроизведён либо гипотеза опровергнута и найдена другая причина; baseline и правила переходов записаны, ограничения доказательств явно указаны.

## Блок 2 — Планирование по фактическому наличию данных

Классификация должна различать следующие результаты:

| Evidence | Действие |
| --- | --- |
| Пригодный checkpoint и неотозванный subset | Существующий validated restore, без повторного startup hashing |
| Заведомо отсутствующий файл/диапазон нового download | Incomplete, без payload hash attempt; download по существующим gates |
| Существующие bytes без trust, включая upgrade/no checkpoint | Ограниченная hash очередь; positive readiness только после успеха |
| Файл короче конца piece | Piece incomplete; не доверять даже присутствующей части; другие полностью покрытые pieces можно проверить |
| Изменение evidence существующего trusted файла | Отзыв затронутого subset с учётом boundaries, затем проверка доступного содержимого |
| Ошибка доступа, неизвестная inventory, volume mismatch или recovery block | Не выдавать отсутствие за достоверный negative result; сохранить диагностику и safety gates |

- Получать rooted evidence bounded file batches, а не выполнять одинаковый stat на каждый отсутствующий piece. Отсутствие hints/checkpoint само по себе не доказывает отсутствие payload.
- Negative evidence привязать к backend open/mapping/attachment и mutation generation. Начало создания/записи файла должно инвалидировать соответствующее отрицательное состояние: новый полностью полученный piece обязательно попадает на hash-проверку.
- Учитывать v1 pieces на границе нескольких файлов, v2 alignment/padding, hybrid aliases, zero-length, `.part` и final names. Нельзя пропустить потенциально существующее содержимое только из-за отсутствия одного варианта имени.
- Size не выявляет дырки sparse-файла: покрытые логическим размером диапазоны без trust остаются кандидатами hashing. Не трактовать отсутствие физических blocks как доказательство нулевого/невалидного содержимого.
- Проход планирования масштабируется как O(files + pieces), без payload reads для достоверно отсутствующих extents. Использовать существующие limits; file batches ≤128, scratch ≤128 KiB, без goroutine на piece/file. Дополнительную память ограничить и измерить до выбора новых структур.

**Приёмка:** новый отсутствующий payload не создаёт startup hash backlog/поток ENOENT; ни один skip не становится complete. Создание файла после negative scan и получение данных приводят к обязательному hashing, существующие данные без checkpoint не теряются.

## Блок 3 — Согласование проверки с параллельной записью

- Исправить установленную в блоке 1 причину ошибок, а не подавлять `storage changed during verification` и не отключать сравнение identity/evidence.
- Нормальная внутренняя запись в другой piece того же файла не должна отвергать неизменённый проверенный диапазон только из-за обновления file-wide mtime/size. Сохранить detection внешних изменений в пределах документированной модели доверия.
- Write в проверяемый диапазон, replace/truncate/import/promotion, invalidation и смена attachment должны отменять/отвергать устаревший результат. Ни изменение подписи «в пользу» проверки, ни повторный callback не могут вернуть отозванный trust.
- Устранить TOCTOU между чтением evidence, внутренним write accounting и публикацией completion. Не удерживать library/global/coordinator locks через stat/read/flush/SQL или ожидание другого worker; использовать существующий порядок lifecycle/payload/fresh locks и расширять его только с документированным обоснованием.
- Не сериализовать всю загрузку файла на время hash backlog. Retry только для действительно устаревшего результата, с coalescing и bounded admission; нет busy loop, starvation download/streams и потерянных задач при saturation/cancel.
- Durable revocation остаётся до mutation, seal ждёт соответствующие writers/hash/completion/layer updates. Внутреннее обновление evidence не должно оживлять старый checkpoint или распространять trust на соседние непроверенные bytes.

**Приёмка:** barrier tests доказывают допустимость результата при независимой записи и отказ при изменении того же диапазона; параллельная загрузка/проверка/стриминг завершаются без ложных конфликтов и deadlock.

## Блок 4 — Приоритеты, диагностика и совместимость

- Проверить, что вновь полученный полный piece и диапазон активного stream не ждут полного фонового прохода существующих файлов. Использовать текущий bounded scheduler, сохраняя fairness и отсутствие голодания фоновой проверки.
- Не подменять stream-ready проверкой всего торрента. Direct file path требует прежних гарантий полного файла, torrent reader может читать только допустимые pieces; upload использует те же правила доверия.
- Различать проверенное хешем содержимое, trusted restore, negative skips и ожидающую работу. Skipped-missing bytes не увеличивают downloaded/freshly-verified/restored counters; завершённое планирование не означает successful hash или selection complete.
- При необходимости добавить additive cached REST/SSE counters/причины и согласованные Go/TS fixtures. Не менять существующие wire-смыслы молча; API polling не делает stat/hash/flush. GUI не показывает пустой торрент как бесконечную активную проверку.
- ENOENT ожидаемого `not_created` агрегировать в ограниченную summary, а не WARN на каждый piece. Реальные access/I/O/signature failures оставлять видимыми с причиной и owner/attachment; логи не содержат credentials. Отменённый клиентом stream не выдавать за установленную порчу данных.

**Приёмка:** локальный seeder + HTTP Range дают первые проверенные bytes до завершения фоновой проверки; counters честны, TorrServer и Web GUI совместимы, polling не создаёт storage I/O.

## Блок 5 — Регрессии, измерения и выпуск

Обязательная матрица:

- Новый отсутствующий torrent; create между scan/hash/write; частичный/truncated/sparse файл; существующие валидные/повреждённые данные без checkpoint; `fast`, `verify`, explicit recheck.
- Valid/invalid/revoked checkpoint, clean/crash reopen, внешний replace/mtime/size change; same-size/same-mtime limitation в fast документирована, strict recheck обнаруживает порчу.
- V1 shared boundaries, v2/hybrid/layers, aliases, Skip/смена selection, zero-length, `.part`/promotion/import.
- Соседние и совпадающие write/hash, stale stat/callback/token, refresh/revoke, periodic seal, отмена, queue saturation, Pause/Resume, DiskFull, move/locate/repair/delete/re-add и shutdown join.
- MissingFiles/permissions/volume/journal gates без автоматического repair. Существующие trusted read/upload/HTTP direct/archive paths не обходят invalidation.
- Детерминированные counters: на пустом fixture ноль startup payload hash reads и hash attempts для missing pieces, O(files) probes без per-piece ENOENT; каждый полученный piece проверен до read/upload; независимые writes не дают ложного completion conflict.
- Сравнить baseline на одинаковых fixtures минимум в трёх прогонах. Отдельно показать startup, foreground stream latency, download throughput, filesystem/SQL amplification и память. Не задавать нестабильные CI thresholds по MB/s/секундам; корректность и объём лишней работы проверять counters/barriers.
- Выполнить `make check`: root `go test ./...`, race/vet/build, fork `.`/`./storage` и остальные изменённые пакеты, frontend tests/build. Изменения storage signatures/locking проверить native Linux/macOS/Windows; cross-compile не заменяет execution. Недоступный platform gate оставить открытым.
- Обновить README, fast-resume/storage contracts в затронутой части, `internal/engine/CONCURRENCY.md`, `internal/usecase/STATE.md` и итоговый отчёт. Не переписывать исторические результаты фаз 42–43.

## Definition of Done

- [ ] F44-01: baseline, воспроизведение причины и контракт negative evidence/write/hash publication зафиксированы.
- [x] F44-02: достоверно отсутствующие диапазоны не хешируются при открытии; missing skips не дают trust/readiness и не сохраняются как positive completion.
- [x] F44-03: существующие непроверенные данные проверяются, trusted checkpoint subset восстанавливается по фазе 43; новые полученные pieces обязательно проходят hash перед использованием.
- [x] F44-04: независимые внутренние writes не вызывают ложных completion conflicts; изменение проверяемого диапазона и stale lifecycle results безопасно отвергаются без deadlock/starvation.
- [x] F44-05: streaming/download не ждут полного фонового прохода; bounded budgets, QoS, durable revocations/seal и safety/recovery gates сохранены. Targeted stream/download/fairness/checkpoint suites пройдены; полная финальная приёмка относится к F44-07.
- [x] F44-06: counters/REST/SSE/GUI/logging различают hashing, restore и skips; API polling не выполняет storage I/O, нет WARN на каждый заведомо отсутствующий startup piece. Explicit recheck не переиспользует negative snapshot.
- [ ] F44-07: матрица тестов, root/fork/frontend checks и native platform gates проходят; сравнительные измерения подтверждают устранение лишней работы без ослабления integrity.
- [x] Документация и `tasks/implementation/phase44-report.md` обновлены с точными evidence, ограничениями и отклонениями; пользовательский сервер не изменён.
