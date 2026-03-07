# h3ws2h1ws-proxy

Прокси-сервер для WebSocket over HTTP/3 (RFC 9220) на входе и классического WebSocket over HTTP/1.1 на backend.

## Что умеет проект

- Принимать WebSocket-подключения через **HTTP/3 extended CONNECT**.
- Проксировать трафик между клиентом (H3 WS) и backend-сервисом (`ws://` или `wss://`).
- Ограничивать размер:
  - отдельного WebSocket frame (`max-frame`),
  - итогового сообщения (`max-message`).
- Ограничивать общее число одновременных сессий (`max-conns`).
- Проксировать control frames (`ping`, `pong`, `close`) и корректно завершать сессии.
- Отдавать Prometheus-метрики на отдельном HTTP endpoint (`/metrics`).
- Отдавать health-check endpoints для проверки живости (`/health/tcp`, `/health/udp`).

## Архитектура и назначение модулей

### `cmd/h3ws2h1ws-proxy/main.go`
Минимальная точка входа: вызывает `app.Run()` и завершает процесс при ошибке.

### `internal/run.go`
Bootstrap приложения:
- парсинг флагов,
- валидация backend URL,
- запуск metrics endpoint,
- создание и запуск HTTP/3 сервера.

### `internal/config/config.go`
Содержит:
- `Config` — конфигурация процесса (адреса, лимиты, таймауты),
- `Limits` — runtime-лимиты прокси,
- `DefaultTLSConfig()` — TLS 1.3 + ALPN для HTTP/3.

### `internal/metrics/metrics.go`
Определяет и регистрирует Prometheus-метрики:
- активные сессии,
- принятые/отклонённые подключения,
- ошибки по стадиям,
- объём и число проксируемых сообщений,
- control frames,
- дропы из-за лимитов.

### `internal/proxy/proxy.go`
Содержит основную логику установки сессии:
- проверка RFC9220-заголовков,
- ответ handshake (`Sec-WebSocket-Accept`),
- dial backend WS,
- запуск двунаправленных pump-потоков,
- жизненный цикл сессии и обработка ошибок.

### `internal/proxy/pumps.go`
Логика передачи данных:
- `pumpH3ToBackend` — из H3-потока в backend WebSocket,
- `pumpBackendToH3` — из backend WebSocket в H3-поток.

Включает:
- сборку фрагментированных сообщений,
- применение таймаутов,
- реакцию на `ping/pong/close`,
- проверки лимитов размеров.

### `internal/ws/framing.go`
Низкоуровневая реализация RFC6455:
- чтение frame (`ReadFrame`),
- запись data/control/close frame,
- фрагментация больших payload,
- маскирование/демаскирование,
- парсинг payload close frame.

### `internal/ws/utils.go`
Вспомогательные функции:
- `ComputeAccept` — расчёт `Sec-WebSocket-Accept`,
- `PickFirstToken` — выбор первого subprotocol,
- `IsNetClose` — эвристика «нормального» закрытия соединения.

## Запуск

### Требования
- Go 1.25+
- TLS-сертификат и ключ (`cert.pem`, `key.pem`) для HTTP/3 сервера.

### Пример

```bash
go run ./cmd/h3ws2h1ws-proxy \
  -listen :443 \
  -cert cert.pem \
  -key key.pem \
  -backend ws://127.0.0.1:8080 \
  -path "^/ws/(tcp|udp)$" \
  -metrics 127.0.0.1:9090
```

### Пример запуска в Docker

```bash
docker build -t h3ws2h1ws-proxy:local .

docker run --rm \
  -p 443:443/udp \
  -p 9090:9090 \
  -v "$(pwd)/cert.pem:/app/cert.pem:ro" \
  -v "$(pwd)/key.pem:/app/key.pem:ro" \
  h3ws2h1ws-proxy:local \
  -listen :443 \
  -cert /app/cert.pem \
  -key /app/key.pem \
  -backend ws://host.docker.internal:8080 \
  -path "^/ws/(tcp|udp)$" \
  -metrics :9090
```

> Для Linux при необходимости добавьте `--add-host=host.docker.internal:host-gateway`, чтобы контейнер мог достучаться до backend на хосте.

## Основные флаги

- `-listen` — UDP адрес HTTP/3 сервера (по умолчанию `:443`)
- `-cert` / `-key` — TLS сертификат и ключ
- `-backend` — URL backend WebSocket (`ws://` или `wss://`) без пути
  - Путь и query всегда берутся из входящего запроса (например, `/ws/tcp?x=1` → `ws://backend/ws/tcp?x=1`).
- `-path` — regexp-маска пути для RFC9220 CONNECT (по умолчанию `^/ws$`)
- `-metrics` — адрес endpoint метрик (по умолчанию выключен; пустое значение отключает сервер метрик)
- `-max-frame` — максимум байт в одном frame
- `-max-message` — максимум байт в одном собранном сообщении
- `-max-conns` — максимум одновременных сессий
- `-read-timeout` / `-write-timeout` — таймауты чтения/записи
- `-debug` — включает подробные debug-логи по QUIC/HTTP3 handshake и потоку проксирования (кадры/сообщения/ошибки)

## Метрики

Endpoint: `http://<metrics-addr>/metrics` (доступен только если задан `-metrics`)

Health-check endpoints (на основном HTTP/3 listener):
- `/health/tcp` → для `GET`: `200 OK` + `ok`; для `CONNECT`: `200 OK` (без тела; закрытие выполняет probe-клиент)
- `/health/udp` → для `GET`: `200 OK` + `ok`; для `CONNECT`: `200 OK` (без тела; закрытие выполняет probe-клиент)

Ключевые метрики:
- `h3ws_proxy_active_sessions`
- `h3ws_proxy_accepted_total`
- `h3ws_proxy_rejected_total{reason=...}`
- `h3ws_proxy_errors_total{stage=...}`
- `h3ws_proxy_bytes_total{dir=...}`
- `h3ws_proxy_messages_total{dir=...,type=...}`
- `h3ws_proxy_frames_total{dir=...,opcode=...}`
- `h3ws_proxy_message_size_bytes_bucket{dir=...,type=...,le=...}`
- `h3ws_proxy_session_duration_seconds_bucket{le=...}`
- `h3ws_proxy_session_traffic_bytes_bucket{dir=...,le=...}`
- `h3ws_proxy_control_frames_total{type=...}`
- `h3ws_proxy_oversize_drops_total{kind=...}`


## Тюнинг под высокую нагрузку

В текущей версии включены базовые оптимизации для production-нагрузки:

- увеличены лимиты QUIC по входящим потокам и receive windows,
- отключена per-message compression при dial в backend (меньше CPU под большим RPS),
- увеличены буферы чтения/записи backend WebSocket dialer до 64 KiB,
- добавлен timeout на backend WebSocket handshake,
- увеличен буфер чтения H3 WebSocket frame-потока до 64 KiB.

Для дальнейшего масштабирования в real-world трафике рекомендуется:

- запускать несколько инстансов за L4/L7 балансировщиком,
- подбирать `-max-conns`, `-max-frame`, `-max-message` под профиль клиентов,
- мониторить p95/p99 latency и `h3ws_proxy_errors_total{stage="session"}`,
- выносить backend WS в отдельный autoscaling-пул.


## Troubleshooting

### Ошибка: `expected first frame to be a HEADERS frame`

Если в логах прокси видно:

- `Application error 0x101 (local): expected first frame to be a HEADERS frame`
- `read response headers failed ... expected first frame to be a HEADERS frame`

это означает, что проблема возникает **до** обработки запроса приложением.

Важно: в `quic-go` эта формулировка может появляться в двух случаях:
- на request stream первым пришёл не `HEADERS`;
- `HEADERS` пришёл, но header block/QPACK не декодируется корректно.

Для RFC 9114 / RFC 9220 корректный порядок на client-initiated bidirectional stream:

1. `HEADERS` (Extended CONNECT, `:method=CONNECT`)
2. далее DATA / WebSocket payload

Что проверить на стороне клиента/шлюза:

- используется именно HTTP/3 framing, а не «сырые» данные сразу в QUIC stream;
- первым кадром request stream идёт `HEADERS`;
- QPACK/заголовки декодируются сервером;
- если используется промежуточный gateway, он не переписывает request stream в несовместимый формат.

Прокси экспортирует метрики для этого класса проблем:

- `h3ws_proxy_prerequest_close_total{reason="request_stream_invalid_first_frame_or_headers"}`
- `h3ws_proxy_prerequest_close_total{reason=...}`
- `h3ws_proxy_errors_total{stage="h3_framing"}`

## Docker + Grafana

Добавлены файлы для локального observability-стека:

- `Dockerfile` — сборка и запуск прокси в контейнере.
- `docker-compose.yml` — запуск `h3ws-proxy`, `prometheus`, `grafana`.
- `deploy/prometheus/prometheus.yml` — scrape-конфиг Prometheus.
- `deploy/grafana/provisioning/datasources/prometheus.yml` — datasource provisioning.
- `deploy/grafana/provisioning/dashboards/dashboards.yml` — auto-import dashboard.
- `deploy/grafana/dashboards/h3ws-proxy-overview.json` — production-ready dashboard (SLO, трафик, размеры сообщений, ошибки).

Запуск:

```bash
docker compose up --build
```

После запуска:

- Grafana: `http://localhost:3000` (`admin/admin`)
- Prometheus: `http://localhost:9091`
- Proxy metrics: `http://localhost:9090/metrics`

> В compose-конфигурации при первом старте автоматически генерируется self-signed TLS сертификат в `./certs`.
