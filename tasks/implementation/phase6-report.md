# Отчет по реализации: Фаза 6 (Deployment & QA)

## Что было сделано
1. Добавлена точка входа приложения `cmd/hub/main.go`:
   - Загружает конфигурацию из ENV/флагов.
   - Создает runtime-директории для загрузок и SQLite базы.
   - Инициализирует SQLite, repository, torrent engine, usecase и SSE sync worker.
   - Запускает HTTP-сервер и корректно завершает его по `SIGINT/SIGTERM`.
2. Добавлено встраивание фронтенда через `go:embed`:
   - Создан `web/embed.go`, который встраивает `web/dist` в Go-бинарник.
   - `internal/delivery/http/router.go` обновлен: принимает embedded static FS и раздает SPA fallback для Web GUI.
   - CORS оставлен открытым для локальных Smart TV/браузерных клиентов (`AllowedOrigins: ["*"]`).
3. Добавлены Docker-артефакты:
   - `Dockerfile` с multi-stage сборкой: Node/Vite frontend stage, Go backend stage, финальный Alpine runtime.
   - `.dockerignore` для исключения runtime-данных, `node_modules`, `dist` и лишних файлов из контекста.
   - `docker-compose.yml` с портами `8080`, `50007/tcp`, `50007/udp`, volumes `./downloads:/downloads`, `./config:/config` и ENV-настройками из ТЗ.
4. Обновлен `.gitignore`:
   - Добавлены корневые бинарники `/hub` и `/torrent-stream-hub`, чтобы локальная сборка не попадала в git.
5. Выполнена проверка сборки и тестов:
   - `npm run build` в `web/` успешно собрал `web/dist`.
   - `go test ./...` успешно прошел по всем Go-пакетам.
   - `CGO_ENABLED=0 go build ./cmd/hub` успешно собрал backend с embedded frontend.
   - `docker build -t torrent-stream-hub:test .` успешно собрал Docker-образ.
    - Контейнер был запущен через `docker run`; проверены `/echo` и SPA root `/` через HTTP.
6. Дополнительно после QA-исправлений startup теперь вызывает `uc.RestoreTorrents()`, чтобы persisted торренты из SQLite снова регистрировались в engine после перезапуска приложения. Для новых записей восстановление использует сохраненный `source_uri` с tracker-ами.

## Статус выполнения DoD
- [x] Фронтенд успешно собирается и встраивается в бинарный файл бэкенда через `go:embed`.
- [x] Создан рабочий `Dockerfile`, собирающий единый образ приложения.
- [x] Создан `docker-compose.yml`, позволяющий запустить приложение со всеми нужными настройками.
- [x] Приложение успешно запускается в Docker.
- [ ] Ручное тестирование (VLC/браузер) подтверждает стабильную потоковую передачу.

## Причины отхода от плана
1. Полное ручное тестирование стриминга через VLC/браузер не выполнено в рамках текущего окружения, так как для этого нужен реальный torrent/magnet с доступными пирами или подготовленный локальный сидер с медиафайлом. Проверены Docker-запуск, `/echo` и раздача embedded SPA, но E2E-проверку стабильности `/stream` с `Range`-запросами нужно выполнить отдельно на реальном медиа-торренте.

## Команды проверки
```bash
npm run build
go test ./...
CGO_ENABLED=0 go build ./cmd/hub
docker build -t torrent-stream-hub:test .
docker run --rm -d --name torrent-stream-hub-test -p 18080:8080 -p 55007:50007/tcp -p 55007:50007/udp -e HUB_MIN_FREE_SPACE_GB=0 torrent-stream-hub:test
curl -fsS http://localhost:18080/echo
curl -fsS http://localhost:18080/
docker stop torrent-stream-hub-test
```
