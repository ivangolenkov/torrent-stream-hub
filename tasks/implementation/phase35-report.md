# Фаза 35: Изолированный Payload И Verified Completion — Отчёт

Дата: 2026-10-22. Основание: `tasks/phase35-isolated-storage-and-verified-completion.md`, F05/F06 аудита. **Реализация и DoD завершены**: обязательные прикладные проверки и root/storage тесты/race форка проходят без исключений. Дополнительные libpossum/FUSE проверки ограничены окружением, как допускает исходный план. Фаза 36 не начиналась; в ходе закрытия приёмки commit/push не выполнялись.

## Что Сделано

### F05: Mapping, Изоляция И Writer

- `internal/repository/migrations/0005_storage_layout.sql`, `storage.go`: отдельный durable mapping version/root/state/journal/error, импорт старых записей как `legacy`, резервирование hybrid aliases. Runtime snapshots не перезаписывают mapping.
- `internal/payload/files.go`, `internal/engine/storage_roots.go`, `client_config.go`: общий layout `payload/<stable-infohash>/<relative-metainfo-path>`, проверки reserved/service namespace и сохранённого root; v1/v2/hybrid используют одну стабильную идентичность.
- `internal/usecase/torrent_uc.go`, `temporary_files.go`, `temporary.go`, `internal/engine/stream_source.go`: add/restore/Resume, download/reopen, direct stream и deletion manifests используют mapping, не угадывают пути. Pending deletion и migration journal исключают конфликтующие операции. Исторический процент готовности больше не открывает доступ к файлам.
- `pkg/anacrolix-torrent/storage/file-io-root.go`, `file-links_{unix,windows}.go`, `file-client.go`: фактические open/mkdir/read/write/flush/publication через `os.Root`, запрет symlink/non-regular/hardlink, неоднозначных final/part и case/unicode путей. Публикация не перезаписывает занятый target.
- `internal/models/torrent.go`, repository query/merge, `web/src/types/index.ts`, `TorrentTable.vue`: `storage_state`/`storage_error` в REST/SSE и диагностике строки торрента.

### Безопасная Миграция

- `internal/usecase/storage_migration.go`: последовательный фоновый worker, инвентаризация владения, запрет угадывания source при metadata-pending/неточном metainfo, shared paths, alias и namespace conflicts. Глобальный lifecycle lock не удерживается во время copy/hash.
- До копирования сохраняется journal; source остаётся на месте до commit. SHA256 source/copy, bounded-buffer copy во временный target, fsync, atomic no-replace `Root.Link`/unlink и sync каталогов предшествуют переключению mapping.
- Hybrid aliases резервируются до копирования, чтобы параллельный add не выделил второй root. Допускается безопасный retry после DB failure, cancellation и restart, включая прерывание между link и unlink временной копии.
- После offline hash-прохода частичные/неверные pieces остаются непроверенными. Перенос не превращает повреждённые bytes в готовый файл: рабочий runtime проверяет storage заново и при необходимости докачивает.
- Mapping с журналом не допускается к работе даже после commit. Перед cleanup повторно сверяются **target и source** с journal; изменившийся committed target не разрешает удалить оставшийся source. Очистка завершает journal, затем восстанавливается runtime с сохранёнными policy/FIFO/priorities.
- `internal/engine/verify_storage.go`: offline verifier без discovery/upload/download, проверка свободного места с резервом, tracked background tasks и paused admission для проверки без скачивания.

### F06: Проверенность Текущего Поколения

- `storage/file-torrent.go`, `file-piece.go`, `file-torrent-io.go`: приложение включает `VerifyOnOpen`; fresh completion начинается пустым, old Bolt marks и final name/size не принимаются за hash proof. Старое поведение сторонних клиентов storage без этой опции не меняется.
- Перед hash снимается signature затронутых файлов; перед completion commit проверяются inode/size/mtime. Изменения отзывают trust. Внутренние writes обновляют signatures непересекающихся pieces, не отзывая весь файл из-за изменения общего mtime.
- `torrent.go`, `peerconn.go`, `reader.go`: свежесть проверяется при direct readiness, torrent reads, bitfield/HAVE и upload admission. Responsive reader не обходит verified gate. Resource manager и direct checks обновляют completion и планируют recheck при изменении storage.
- `config.go`, `client_config.go`: глобально не более двух hashers основного клиента; offline migration verifier — один. Payload mmap не добавлялся. I/O проверяет context, lifecycle barrier запрещает поздние write/completion после Drop/Close; metadata watchers и worker входят в engine shutdown.
- `piece.go`/`torrent.go`: `VerifyDataContext` ждёт окончания completion commit, а не только вычисления hash; исправлен ранний выход с удержанным lock при уже закрытом торренте.
- Узкие v2/hybrid исправления в `client.go`/`misc.go`: pure-v2 add без ненулевой v1 hash, регистрация v2 lookup alias при hybrid add, отклонение несовместимого hybrid piece count до panic. Стабильная app identity не меняется после получения второго hash.

## Проверки И Регрессии

Успешные прогоны на macOS arm64:

```sh
go test -count=1 -timeout 90s ./...
go test -race -count=1 -timeout 90s ./internal/... ./cmd/...
go vet ./...
go build -o <approved-temporary-directory>/phase35-hub ./cmd/hub
# Из pkg/anacrolix-torrent:
go test -count=1 -skip '^TestHolepunchConnect$' -timeout 90s . ./storage
go test -race -count=1 -skip '^TestHolepunchConnect$' -timeout 90s . ./storage
# Последний race-прогон после добавления всех regression-тестов:
go test -race -count=1 -skip '^Test(HolepunchConnect|ClientValidTrackerConn)$' -timeout 60s . ./storage
# Последний целевой migration/recovery race-прогон из корня проекта:
go test -race -count=1 -run 'TestStorage|TestHybrid' -timeout 45s ./internal/usecase
# Из web:
npm test
npm run build
```

Последние повторные проверки и ограничения следует читать вместе с разделом ниже: один успешный прогон не означает стабильность всего upstream-набора. `git diff --check` проходит. macOS race-линковка сообщает `malformed LC_DYSYMTAB`, без обнаруженных data races в успешных прогонах.

Ранние tool timeouts с пустыми прикладными логами при повторе не воспроизвелись; их первоначальная причина не установлена. Последний полный прикладной прогон после изменения committed-journal recovery успешен.

Новые/обновлённые тесты:

- `internal/repository/storage_test.go`: upgrade pre-0005 SQLite, legacy mapping, идемпотентность aliases и отказ от переназначения чужого root.
- `internal/usecase/storage_migration_test.go`: v1/v2/hybrid, single/multi, paused/policy/FIFO/priorities, partial/bad-content; shared/occupied/symlink/hardlink/нет metainfo/canceled; DB commit failure и restart, защита pending journal от Delete; copy-publication recovery; free-space reserve/source retention; проверка target при cleanup committed journal; независимость одинаковых имён и hybrid aliases, включая получение metadata v2 magnet.
- `storage/verified_file_test.go`, `verified_bolt_test.go`: старый cache после реального reopen Bolt, отсутствие доверия final size/name, fresh generation при reopen, mutation во время hash и после проверки, late write после Close, boundary pieces multi-file и zero-length file, writer symlink/hardlink containment, overlapping metadata, cancellation/no-replace.
- `verified_reader_test.go`: неверные same-size final bytes, responsive timeout без выдачи corrupt bytes, отсутствие bitfield/all-complete для unverified pieces, готовность правильных bytes после recheck и отзыв после замены.
- HTTP/usecase/engine fixtures переведены на реальные hashes, isolated root и verified runtime, а не DB-only 100%. Существующие download/ZIP/HEAD/Range, deletion/leases и Close regressions входят в корневой прогон.
- `peerconn_test.go`: fixture теперь явно отмечает piece complete перед HAVE, который проверяет completion; bitfield остаётся неизменным после последующего HAVE.

## Закрытие Тестовой Приёмки

После первоначально заблокированной приёмки исправлены только тестовые fixtures; production-логика и границы F05/F06 не расширялись:

- `ut-holepunching_test.go`: этот форк намеренно не рекламирует holepunch в `makeBuiltinLtepProtocols`, поэтому старый fixture ждал недоступную возможность. В fixture расширение включается до peer handshakes, только для трёх тестовых клиентов. IPv4 loopback устраняет неоднозначность адреса rendezvous; PEX на relay отключён, чтобы проверять holepunch независимо от discovery/фильтрации не принимающего соединения seeder. Проверки реальной передачи и holepunch counters сохранены. Исправлена также проверка ошибки создания третьего клиента.
- `client-tracker_test.go`: callback передаёт только ожидаемое событие через bounded неблокирующий канал. Повторные/завершающие события не блокируют shutdown. Assertions выполняются в тестовой goroutine после получения события, вместо общих bool и `require` внутри callback; таймеры останавливаются.
- `fs/torrentfs_test.go`: отсутствие OSXFUSE явно вызывает skip интеграционного теста; остальные ошибки mount остаются ошибками. Прежнее продолжение с nil connection приводило к panic. Это не подтверждает реальную FUSE-интеграцию на данной машине.

Итоговые повторные проверки успешны:

```sh
# Корень проекта:
go test -count=1 -timeout 90s ./...
go test -race -count=1 -timeout 90s ./internal/... ./cmd/...
go vet ./...
go build -o <approved-temporary-directory>/phase35-acceptance-hub ./cmd/hub
# pkg/anacrolix-torrent, без -skip:
go test -count=1 -timeout 90s . ./storage ./fs
go test -race -count=1 -timeout 90s . ./storage ./fs
go test -race -count=5 -run '^Test(HolepunchConnect|ClientValidTrackerConn|ClientInvalidTracker|ClientAnnounceFailure|ClientAnnounceSuccess)$' -timeout 90s .
# web:
npm test
npm run build
# Корень проекта:
git diff --check
```

Логи в approved temporary directory `.../T/opencode/`: `phase35-acceptance-app.log`, `phase35-acceptance-app-race.log`, `phase35-acceptance-fork.log`, `phase35-acceptance-fork-race.log`, `phase35-acceptance-repeat.log`, `phase35-acceptance-fork-all.log`. macOS linker warning остаётся, data races не обнаружены.

Повторный полный `go test -count=1 -timeout 90s ./...` вложенного модуля **не зелёный**: единственный build failure — `storage/possum`, `ld: library 'possum' not found`. Остальные пакеты проходят; `TestUnmountWedged` явно SKIP (`cannot locate OSXFUSE`). Исходный план требует root/storage тесты и race, а полный набор — при доступности внешних зависимостей. Поэтому DoD закрыт без ослабления критериев; прежнее требование безусловно зелёного полного upstream-набора было излишне строгим прочтением плана. Для проверки optional backends в будущем необходимы libpossum и рабочее FUSE-окружение; они приложением не используются.

## Отклонения И Оставшиеся Ограничения

1. Вместо предпочтительного rename миграция всегда копирует source. Это сохраняет исходную копию до durable commit и упрощает безопасный restart. Цена — дополнительное дисковое место и I/O; нужны copy-space reserve и backup, процедура описана в README.
2. Первоначальные таймауты `TestHolepunchConnect` (также на чистом baseline) и `TestClientValidTrackerConn` устранены исправлениями fixtures выше. Ранние прогоны с исключениями сохранены как история, но основанием закрытия служат новые прогоны без `-skip`.
3. Полный набор вложенного модуля ограничен отсутствием libpossum; реальный FUSE mount не проверен из-за отсутствия OSXFUSE. Неиспользуемые приложением optional backends не объявляются проверенными; обязательные root/storage проверки завершены.
4. Фаза 35 больше не блокирует переход по DoD. Реализация фазы 36 в рамках закрытия приёмки не выполнялась.
5. NAS/Smart TV/Windows/Linux runtime-прогоны, реальный power-cut, физическое заполнение диска до ENOSPC, readonly volume и измерение peak RSS на NAS не выполнялись. Проверены reserve rejection, context cancellation и DB failure/restart; это не доказательство всех аппаратных crash boundaries. Не поддерживающая hardlink/fsync файловая система должна отказать в миграции с сохранением source, а не использовать небезопасный fallback.
6. Root containment/stat/hash не являются sandbox против привилегированного локального writer. Во время hash и передачи предполагается отсутствие внешнего writer; изменение bytes с сохранением всех проверяемых атрибутов одним stat не обнаружить. Для обычной storage publication авария между link/unlink может оставить две hardlink-ссылки: backend консервативно отвергает их, автоматического выбора владельца нет; восстанавливать только offline после backup. Migration temp links восстанавливаются по журналу.
7. Старые значения статистики могут оставаться историческими; verified gate их не использует. Полный редизайн статистики, приоритеты/DiskFull фазы 36, auth и dependency upgrades не входили в эту фазу.

## Definition Of Done

- [x] F05: разные раздачи изолированы; writer/reader/delete используют versioned mapping.
- [x] Возобновляемая legacy migration без перезаписи чужих target, блокировка неоднозначных случаев и README recovery.
- [x] Policy/FIFO/metadata/priorities/manifests сохранены, pending deletion не мигрируется.
- [x] F06: final name/size и старый cache не дают verified status после открытия.
- [x] Direct/download/ZIP/torrent reader/seeding используют verified gate в исправленных сценариях.
- [x] Ограниченные hashers, отмена/Close barrier, без payload mmap.
- [x] Тестовая приёмка по исходному плану закрыта: прикладные проверки, root/storage тесты и race форка проходят без исключений; ограничения optional libpossum/FUSE явно отражены выше.
- [x] README backup/migration/recovery/downgrade и этот отчёт созданы.
