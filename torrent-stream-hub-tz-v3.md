# Техническое задание (ТЗ): Гибридный торрент-клиент со стриминг-сервером (Torrent-Stream-Hub)

## 1. Цель проекта
Разработать серверное приложение, объединяющее функциональность классического торрент-клиента (загрузка файлов на диск, сидирование, управление очередью) с возможностями потоковой передачи видео по HTTP с поддержкой перемотки. Приложение должно эмулировать API TorrServer для интеграции с существующими Smart TV приложениями (Lampa, Num, vKino) и предоставлять собственный REST API для управления через Web GUI.

## 2. Основные роли и сценарии использования
* **Сценарий «ТВ-клиент» (Стриминг):** Пользователь открывает фильм в ТВ-приложении. Приложение отправляет magnet-ссылку/торрент в сервис. Сервис начинает последовательную загрузку (Sequential) нужного файла, приоритезируя куски по `Range`-запросам от плеера, и одновременно сохраняет файл на диск. При выключении плеера загрузка переводится обратно в фоновый режим (rarest-first).
* **Сценарий «Торрент-клиент» (Хранение):** Пользователь заходит в Web GUI, добавляет торрент. Сервис скачивает файлы на диск в фоне по классическому алгоритму, после чего остается на раздаче.
* **Сценарий «Медиатека»:** Пользователь видит в Web GUI список всех файлов на сервере, может управлять ими (удалять) и запускать стриминг прямо в браузере.

## 3. Нефункциональные требования и ограничения MVP
* **Сетевое взаимодействие:** Приложение ориентировано на работу в локальной сети. Настройки CORS должны разрешать запросы со всех источников (`*`) для бесшовной интеграции с браузерными плеерами и Smart TV.
* **Ограничения стриминга (MVP):** Поддерживается до 4 одновременных стримов (настраивается через ENV). При этом строгая приоритезация QoS (подавление фоновых загрузок в пользу стрима) гарантируется только для одного основного потока.
* **Производительность:** Минимальная буферизация и быстрый старт воспроизведения даже на слабых устройствах (NAS, одноплатные компьютеры).

## 4. Архитектура и технологический стек
* **Backend:** Язык Go. Обеспечивает компиляцию в единый бинарный файл, высокую производительность сети и низкое потребление памяти.
* **Торрент-движок:** Библиотека `github.com/anacrolix/torrent`.
* **База данных:** SQLite. Хранение метаданных раздач, настроек и истории просмотров. Использование режима `PRAGMA journal_mode=WAL;` и пула соединений обязательно для предотвращения блокировок при фоновой синхронизации.
* **Frontend:** Vue 3 (Composition API) + TypeScript + Vite. State management через Pinia. UI на базе Tailwind CSS. Реалтайм обновления через Server-Sent Events (SSE).

## 5. Модель данных и состояния
Для корректной синхронизации бэкенда и рендеринга Web GUI вводится строгий конечный автомат.

### 5.1. Состояния торрента
* `Queued`: В очереди на загрузку (достигнут лимит активных загрузок).
* `Downloading`: В процессе скачивания.
* `Streaming`: Активен стриминг файла(ов) из раздачи.
* `Seeding`: Загрузка завершена, идет раздача.
* `Paused`: Остановлен пользователем или системой.
* `Error`: Критическая ошибка (требуется вмешательство).
* `MissingFiles`: Файлы удалены с диска, ожидается Recheck.
* `DiskFull`: Недостаточно свободного места, загрузка приостановлена.

### 5.2. Модель ошибок
При переходе в статус `Error` или `DiskFull`, система должна отдавать детализированную причину: `Invalid torrent`, `Tracker unreachable`, `No peers`, `Disk full`, `Missing files`.

## 6. Требования к внутреннему Торрент-движку (Engine)

### 6.1. Хранение данных и управление диском
* Прямая запись на жесткий диск (без кастомного кэша в оперативной памяти). Рекомендуется реализация предварительной аллокации (fallocate/Preallocate) для снижения фрагментации. Ограничение использования `mmap` для снижения потребления ОЗУ при тяжелых релизах.
* **Защита диска (Disk Pressure):** При падении свободного места ниже порога (`HUB_MIN_FREE_SPACE_GB`), все активные загрузки автоматически ставятся на паузу со статусом `DiskFull`, новые падают в `Queued`. Сидирование существующих файлов продолжается. Возобновление требует освобождения места и ручного старта.
* Приложение должно корректно обрабатывать ручное удаление/перемещение файлов ОС. Статус меняется на `MissingFiles`.

### 6.2. Жизненный цикл загрузки и управление потоками
Управление приоритетами строится на паттерне подсчета ссылок (Reference Counting) с привязкой к контексту HTTP-запроса (`context.Context`).

1. **Фон:** Обычные загрузки идут по алгоритму rarest-first. Лимитируются переменной `HUB_MAX_ACTIVE_DOWNLOADS`.
2. **Стриминг (Инициализация):** При обращении к `/stream` счетчик активных потоков файла (`activeStreams`) увеличивается. Если счетчик перешел от 0 к 1, движок включает эгоистичный режим (Sequential mode) и ставит высший приоритет первому и последнему кускам (для MOOV-атомов метаданных).
3. **Перемотка (Debounce):** При обрыве соединения плеером (например, при Seek-запросе) запускается таймер задержки (5–10 секунд). Если в течение этого времени поступает новый запрос к файлу с новым `Range`, таймер отменяется, файл остается в Sequential mode.
4. **Завершение стриминга:** Если таймер истек (пользователь действительно выключил плеер), счетчик обнуляется. Снимаются Sequential-приоритеты, файл возвращается в фоновую загрузку rarest-first.
5. **Достижение 100% скачивания:** Как только стриминговый файл скачивается полностью, воспроизведение идет локально с диска. Снимаются искусственные ограничения QoS, и движок продолжает докачивать остальные файлы раздачи в штатном режиме (естественный pre-fetch).

### 6.3. Эмуляция кэша (`/cache`)
Реализуется механизм "скользящего окна" непрерывно скачанных кусков на диске:
* Вычисляется текущий индекс куска на основе `offset` от плеера.
* Проверяется `bitfield` торрента вперед от текущего индекса для подсчета непрерывной цепочки скачанных кусков.
* Количество переводится в байты (`virtualCacheBytes`).
* **Ограничение (Fake Limit):** Вводится искусственный лимит (настраивается через `HUB_STREAM_CACHE_SIZE`, по умолчанию 200 МБ). API возвращает `Math.min(virtualCacheBytes, HUB_STREAM_CACHE_SIZE)`.
* При перемотке в нескачанную зону кэш моментально сбрасывается в 0.

## 7. Спецификация API (OpenAPI 3.0)

```yaml
openapi: 3.0.3
info:
  title: Torrent-Stream-Hub API
  version: 1.0.0
servers:
  - url: /
paths:
  # ==========================================
  # СЛОЙ СОВМЕСТИМОСТИ (TORRSERVER API)
  # ==========================================
  /echo:
    get:
      summary: Healthcheck для ТВ-клиентов
      responses:
        '200':
          description: Успешный ответ
          content:
            text/plain:
              schema:
                type: string
                example: "1.2.133"

  /torrents:
    post:
      summary: Универсальный эндпоинт TorrServer (добавление/список)
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties:
                action:
                  type: string
                  enum: [add, list]
                link:
                  type: string
      responses:
        '200':
          description: Метаданные торрента

  /stream:
    get:
      summary: HTTP Стриминг видео с поддержкой Range-запросов
      parameters:
        - in: query
          name: link
          schema:
            type: string
        - in: query
          name: hash
          schema:
            type: string
        - in: query
          name: index
          schema:
            type: integer
      responses:
        '206':
          description: Частичный контент (поток)
        '200':
          description: Полный файл

  /play/{hash}/{id}:
    get:
      summary: Алиас для воспроизведения по хэшу и индексу файла
      parameters:
        - in: path
          name: hash
          required: true
          schema:
            type: string
        - in: path
          name: id
          required: true
          schema:
            type: string
      responses:
        '200':
          description: Поток данных или 206 Partial Content

  /torrent/upload:
    post:
      summary: Добавление .torrent файла
      requestBody:
        required: true
        content:
          multipart/form-data:
            schema:
              type: object
              properties:
                file:
                  type: string
                  format: binary
      responses:
        '200':
          description: Статус торрента

  /settings:
    post:
      summary: Заглушка настроек сервера для обратной совместимости
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
      responses:
        '200':
          description: Конфигурация сервера

  /viewed:
    post:
      summary: Управление историями просмотров (синхронизация таймкодов)
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties:
                action:
                  type: string
                  enum: [set, rem, list]
                hash:
                  type: string
                file_index:
                  type: integer
      responses:
        '200':
          description: Ок

  /cache:
    post:
      summary: Статус буфера предзагрузки (скользящее окно)
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
      responses:
        '200':
          description: Состояние кэша
          
  /playlist:
    get:
      summary: Генерация M3U плейлиста для сериалов
      parameters:
        - in: query
          name: hash
          required: true
          schema:
            type: string
      responses:
        '200':
          description: Файл audio/x-mpegurl

  # ==========================================
  # MANAGEMENT REST API (ДЛЯ WEB GUI)
  # ==========================================
  /api/v1/torrents:
    get:
      summary: Получить список всех торрентов
      responses:
        '200':
          description: Массив объектов торрентов

  /api/v1/torrent/add:
    post:
      summary: Добавить новый торрент через GUI
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required:
                - link
              properties:
                link:
                  type: string
                save_path:
                  type: string
                sequential:
                  type: boolean
                  default: false
      responses:
        '200':
          description: Ок

  /api/v1/torrent/{hash}/action:
    post:
      summary: Управление жизненным циклом (pause, resume, delete, recheck)
      parameters:
        - in: path
          name: hash
          required: true
          schema:
            type: string
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              properties:
                action:
                  type: string
                  enum: [pause, resume, delete, recheck]
                delete_files:
                  type: boolean
      responses:
        '200':
          description: Ок

  /api/v1/torrent/{hash}/files:
    get:
      summary: Получить дерево файлов раздачи с приоритетами
      parameters:
        - in: path
          name: hash
          required: true
          schema:
            type: string
      responses:
        '200':
          description: Массив файлов

  /api/v1/events:
    get:
      summary: Server-Sent Events (SSE) для обновления интерфейса
      responses:
        '200':
          description: Поток данных (text/event-stream)
```

## 8. Требования к Web GUI
* **Дашборд:** Графики скоростей и общая статистика сервера.
* **Таблица:** Грид загрузок (имя, прогресс, статус, скорости, ETA, сиды/пиры). Наличие бейджа "Стримится" для активных потоков. Учет новых статусов (`DiskFull`, `Queued` и др.).
* **Инспектор (Детали):** Вкладка файлов с возможностью изменения приоритета (Нормальный/Высокий/Не качать). Кнопка "Play" напротив медиафайлов для стриминга в браузере.
* **Управление:** Модальное окно добавления с поддержкой Drag-and-Drop, выбором директории и возможностью частичной загрузки.
* **Связь с сервером:** Обновление статусов загрузок через Server-Sent Events (SSE).

## 9. Структура проекта (Директории)

### 9.1. Backend (Go Clean Architecture)
```text
torrent-stream-hub/
├── cmd/hub/main.go             
├── internal/
│   ├── config/config.go        # Парсинг конфигурации (флаги, env)
│   ├── models/torrent.go       # Доменные структуры и константы состояний
│   ├── engine/                 # Фасад anacrolix/torrent
│   │   ├── engine.go           # Управление ядром и лимитами загрузок/диска
│   │   ├── stream.go           # Управление Context, activeStreams и Debounce
│   │   └── cache_emul.go       # Логика скользящего окна на диске
│   ├── repository/             
│   │   ├── sqlite.go           
│   │   └── migrations/         
│   ├── usecase/                
│   │   ├── torrent_uc.go       
│   │   └── sync_worker.go      
│   └── delivery/http/          
│       ├── router.go           # Настройка роутов, CORS и Auth
│       ├── api/handlers.go     
│       └── torrserver/         
├── storage/                    
├── deploy/                     
└── Makefile                    
```

### 9.2. Frontend (Vue 3)
```text
web/
├── src/
│   ├── api/client.ts           
│   ├── assets/                 
│   ├── components/             
│   ├── layouts/MainLayout.vue  
│   ├── stores/torrentStore.ts  
│   ├── types/index.ts          # Интерфейсы состояний и ошибок
│   └── views/                  
├── vite.config.ts              
└── package.json
```

## 10. Развертывание и конфигурация
Сборка осуществляется через Multi-stage Dockerfile в единый бинарный артефакт, где статика Vue.js встраивается в Go-бинарник через `//go:embed`.

**docker-compose.yml (Пример с полной конфигурацией):**
```yaml
version: '3.8'
services:
  torrent-hub:
    image: torrent-stream-hub:latest
    container_name: torrent-hub
    restart: unless-stopped
    ports:
      - "8080:8080"      # Web UI & API
      - "50007:50007"    # BitTorrent TCP
      - "50007:50007/udp"# BitTorrent UDP
    volumes:
      - ./downloads:/downloads    
      - ./config:/config          
    environment:
      # Базовые пути и порты
      - HUB_PORT=8080
      - HUB_DOWNLOAD_DIR=/downloads
      - HUB_DB_PATH=/config/hub.db
      # Лимиты и управление ресурсами
      - HUB_MAX_ACTIVE_STREAMS=4
      - HUB_MAX_ACTIVE_DOWNLOADS=5
      - HUB_MIN_FREE_SPACE_GB=5
      - HUB_DOWNLOAD_LIMIT=0      # 0 = безлимит (байт/с)
      - HUB_UPLOAD_LIMIT=0
      - HUB_STREAM_CACHE_SIZE=209715200 # 200 МБ в байтах
      # BitTorrent engine core
      - HUB_BT_SEED=true
      - HUB_BT_NO_UPLOAD=false
      - HUB_BT_CLIENT_PROFILE=qbittorrent
      - HUB_BT_RETRACKERS_MODE=append
      - HUB_BT_RETRACKERS_FILE=/config/trackers.txt
      - HUB_BT_DISABLE_DHT=false
      - HUB_BT_DISABLE_PEX=false
      - HUB_BT_DISABLE_UPNP=false
      - HUB_BT_DISABLE_TCP=false
      - HUB_BT_DISABLE_UTP=false
      - HUB_BT_DISABLE_IPV6=false
      - HUB_BT_ESTABLISHED_CONNS_PER_TORRENT=120
      - HUB_BT_HALF_OPEN_CONNS_PER_TORRENT=80
      - HUB_BT_TOTAL_HALF_OPEN_CONNS=1000
      - HUB_BT_PEERS_LOW_WATER=500
      - HUB_BT_PEERS_HIGH_WATER=1200
      - HUB_BT_DIAL_RATE_LIMIT=60
      - HUB_BT_SWARM_WATCHDOG_ENABLED=true
      - HUB_BT_SWARM_CHECK_INTERVAL_SEC=60
      - HUB_BT_SWARM_REFRESH_COOLDOWN_SEC=180
      - HUB_BT_SWARM_MIN_CONNECTED_PEERS=8
      - HUB_BT_SWARM_MIN_CONNECTED_SEEDS=2
      - HUB_BT_SWARM_STALLED_SPEED_BPS=32768
      - HUB_BT_SWARM_STALLED_DURATION_SEC=180
      - HUB_BT_SWARM_BOOST_CONNS=120
      - HUB_BT_SWARM_BOOST_DURATION_SEC=300
      - HUB_BT_SWARM_PEER_DROP_RATIO=0.45
      - HUB_BT_SWARM_SEED_DROP_RATIO=0.45
      - HUB_BT_SWARM_SPEED_DROP_RATIO=0.35
      - HUB_BT_SWARM_PEAK_TTL_SEC=1800
      - HUB_BT_SWARM_HARD_REFRESH_ENABLED=true
      - HUB_BT_SWARM_HARD_REFRESH_COOLDOWN_SEC=900
      - HUB_BT_SWARM_HARD_REFRESH_AFTER_SOFT_FAILS=1
      - HUB_BT_SWARM_HARD_REFRESH_MIN_TORRENT_AGE_SEC=60
      - HUB_BT_SWARM_DEGRADATION_EPISODE_TTL_SEC=900
      - HUB_BT_SWARM_RECOVERY_GRACE_SEC=180
      - HUB_BT_CLIENT_RECYCLE_ENABLED=true
      - HUB_BT_CLIENT_RECYCLE_COOLDOWN_SEC=900
      - HUB_BT_CLIENT_RECYCLE_AFTER_HARD_FAILS=1
      - HUB_BT_CLIENT_RECYCLE_MIN_TORRENTS=1
      - HUB_BT_CLIENT_RECYCLE_MAX_PER_HOUR=2
      # Безопасность (опционально)
      - HUB_AUTH_ENABLED=false
      - HUB_AUTH_USER=admin
      - HUB_AUTH_PASSWORD=admin
      # Права доступа
      - PUID=1000
      - PGID=1000
```

## 11. Развитие проекта (Post-MVP)
1. **Multi-stream Priority:** Интеллектуальное распределение пропускной способности канала при нескольких одновременно запущенных стримах.
2. **Встроенный DLNA-сервер (UPnP AV):** Анонсирование сервиса в локальной сети и переадресация устройств на эндпоинт `/stream` для просмотра на legacy-телевизорах.
3. **Глобальная балансировка (Auto-QoS):** Автоматическая плавная деприоретизация фоновых раздач при активном стриминге для исключения буферизации.
4. **Уведомления:** Поддержка Webhooks и отправка статусов об успешном завершении загрузок в Telegram-бот.
5. **Private-safe tracker policy:** Режимы управления retrackers для приватных раздач: не добавлять public retrackers к private torrents, поддержать политики `public`, `private-safe`, `strict`.
