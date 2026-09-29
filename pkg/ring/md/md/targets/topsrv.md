---
name: topsrv
description: topsrv через api_call — JSON-RPC за одним путём, методы по хостам и их IP, метрикам, алертам, инвентарю и логам nginx; метка instance, время в params, ошибки при HTTP 200.
---

# Таргет `topsrv` — хосты, их метрики и алерты

Мониторинг машин, на которых живут сервисы: CPU, память, диски, сеть, PostgreSQL,
nginx/angie, S.M.A.R.T., сертификаты, пакеты и уязвимости, обращения ботов и весь
трафик сайтов. Не сервисы и не их логи: за `app_*`-метриками — `prom`, за логами
приложений — `grafana`.

Один путь, только `POST`, JSON-RPC 2.0: метод и параметры — в `body`. Имена методов
регистронезависимы. До 10 вызовов можно отправить одним батчем — массивом запросов.
Проект берётся из токена, параметра проекта у методов нет.

`DefaultJQ` снимает конверт и поднимает ошибку наверх; у PromQL-ответов сворачивает
метки в объект и точки в `[ts, value]`, как у Prometheus; у правил алертов оставляет код,
выражение и порог. Нужно всё — `jq: "."`.

## Разрешённые пути и методы

| Путь | Зачем |
|---|---|
| `/api/v1/rpc/` | единственный; метод — в теле, список ниже |

| Метод | Параметры | Зачем |
|---|---|---|
| `meta.whoami` | — | проект, скоупы и срок токена — проверка, что таргет отвечает |
| `host.list` | — | все хосты: hostname, ОС, ядро, версия агента, `lastSeen`, адреса интерфейсов `addresses` |
| `host.get` | `hostname` | один хост с теми же полями или `host_not_found` |
| `host.summary` | — | строка на хост: cpu, memUsed/memTotal, diskMax (%), load, uptime — первый взгляд |
| `metric.query` | `query` | мгновенный PromQL |
| `metric.queryRange` | `query`, `start`, `end`, `step` | ряд за период |
| `alert.rules` | — | действующие правила: код, PromQL, компаратор, порог, `forSeconds`; у горящих — `currentState` и `activeEventId` |
| `alert.list` | `state`, `severity`, `host`, `page`, `pageSize` | события алертов, новые первыми; `state`: pending, firing, resolved |
| `inventory.hosts` | — | ОС, ядро, число пакетов, `cveCount`/`fixableCount` по последнему аудиту |
| `inventory.findings` | `hostname` | открытые уязвимости хоста: пакет, версия, CVE, где исправлено |
| `botlog.search` | `filter`, `limit`, `offset` | обращения ботов к nginx: бот, URI, статус, `verifyState` |
| `weblog.search` | `filter`, `sortDir`, `limit`, `offset` | сырые запросы к сайтам: адрес, URI, статус, время, `requestId` |
| `weblog.summary` | `filter` | итог по выборке: сколько запросов, `avgMs`/`p50Ms`/`p95Ms`, `c2xx`…`c5xx`, `dropped` |
| `weblog.top` | `groupBy`, `filter`, `sortBy`, `sortDir`, `limit` | та же выборка группами: кто и что в топе |
| `weblog.networks` | `filter`, `sortBy`, `minIPs`, `limit` | по автономным системам: пул для скрейпинга видно только здесь |
| `weblog.blockImpact` | `ip`, `userAgent`, `window` | во что обойдётся блокировка: адрес, UA, оба, `/24`, весь ASN |

Всё остальное, включая `alert.silence`, не проходит: метода нет в allowlist, запрос вернёт
`RPCMethodNotAllowed` со списком разрешённых.

## Примеры

```
api_call(calls: [{target: "topsrv", method: "POST", path: "/api/v1/rpc/",
                  body: "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"host.summary\"}"}])
```

Тела, которые чаще всего нужны:

```json
{"jsonrpc":"2.0","id":1,"method":"alert.list","params":{"state":"firing","pageSize":50}}
{"jsonrpc":"2.0","id":1,"method":"metric.query","params":{"query":"topsrv_filesystem_bytes{instance=\"<host>\",type=\"used\"} / topsrv_filesystem_bytes{type=\"total\"} * 100"}}
{"jsonrpc":"2.0","id":1,"method":"metric.queryRange","params":{"query":"100 * (1 - avg by (instance) (rate(topsrv_cpu_seconds_total{mode=\"idle\"}[5m])))","start":"now-6h","end":"now","step":"1m"}}
{"jsonrpc":"2.0","id":1,"method":"inventory.findings","params":{"hostname":"<host>"}}
{"jsonrpc":"2.0","id":1,"method":"botlog.search","params":{"filter":{"from":"now-24h","statusClass":["5xx"]},"limit":100}}
{"jsonrpc":"2.0","id":1,"method":"weblog.summary","params":{"filter":{"from":"now-6h","host":["<домен>"]}}}
{"jsonrpc":"2.0","id":1,"method":"weblog.top","params":{"groupBy":"path","sortBy":"effort","filter":{"from":"now-1h","statusClass":["5xx"]},"limit":20}}
{"jsonrpc":"2.0","id":1,"method":"weblog.networks","params":{"filter":{"from":"now-24h"},"sortBy":"uaPerIp","minIPs":20}}
{"jsonrpc":"2.0","id":1,"method":"weblog.search","params":{"filter":{"requestId":["<из лога сервиса>"]}}}
[{"jsonrpc":"2.0","id":1,"method":"host.summary"},{"jsonrpc":"2.0","id":2,"method":"alert.list","params":{"state":"firing"}}]
```

## Адреса хостов

У каждого хоста из `host.list` и `host.get` есть `addresses`: `address`, `interface`,
`visibility` (`private` — RFC1918, CGNAT; иначе `public`) и `network` — адрес с маской
интерфейса. Это ответ на «чья это машина — `10.0.0.6`» из DSN, конфига или лога:
адрес ищется здесь, а не выводится из меток метрик. Найденный `hostname` — это и
`instance` в PromQL.

```
api_call(calls: [{target: "topsrv", method: "POST", path: "/api/v1/rpc/",
                  body: "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"host.list\"}",
                  jq: ".error // [.result[] | select(any(.addresses[]; .address == \"10.0.0.6\")) | .hostname]"}])
```

Свой `jq` заменяет `DefaultJQ`, поэтому конверт снимается в нём же, а `.error //`
не даёт отказу выглядеть пустым списком.

- Отдаются IPv4, по одному на интерфейс, за те же 48 часов, что и сам список хостов.
  Loopback, link-local и мосты Docker (`docker0`, `docker_gwbridge`, `br-…`) отброшены:
  они одинаковы на многих машинах и ничего не различают.
- **Пустой `addresses` не доказывает, что адреса нет:** адреса приходят отдельным
  запросом, и его отказ вызов не валит — хост приезжает с `[]`.
- **`network` — маска интерфейса, а не подсеть площадки.** В облаках приватный адрес
  нередко висит как `/32` (`10.0.1.20/32`), и отбор по `network == "10.0.1.0/24"` там
  ничего не найдёт; надёжнее по `address` — `startswith("10.0.1.")`.
- На 2026-09-29 мост Nomad (`interface: "nomad"`, по умолчанию `172.26.64.1`) ещё
  приезжает. Он одинаков на всех хостах с `bridge`-сетью у заданий — машину по нему
  не определить.

## Метрики

Хост в PromQL — метка **`instance`**, и это `hostname` из `host.list`. Метки `hostname`
у метрик нет (кроме `topsrv_host_info`): `{hostname="…"}` даёт пустой результат без ошибки.

Каталог метрик и готовые запросы — в репозитории агента:
[docs/metrics.md](https://github.com/vmkteam/topsrv/blob/master/docs/metrics.md) и
[docs/promql-recipes.md](https://github.com/vmkteam/topsrv/blob/master/docs/promql-recipes.md).
Семейства: `topsrv_cpu_*`, `topsrv_memory_bytes{type}`, `topsrv_load_average{interval}`,
`topsrv_filesystem_bytes{mountpoint,type}`, `topsrv_disk_*`, `topsrv_network_*`,
`topsrv_netstat_*`, `topsrv_process_*{group}`, `topsrv_pg_*`, `topsrv_nginx_*`,
`topsrv_angie_*`, `topsrv_smart_*`, `topsrv_ssl_certificate_*`, `topsrv_packages_*`.

Что спрашивают чаще всего:

```promql
100 * (1 - avg by (instance) (rate(topsrv_cpu_seconds_total{mode="idle"}[5m])))
topsrv_memory_bytes{type="available"} / topsrv_memory_bytes{type="total"} * 100
topsrv_filesystem_bytes{type="used"} / on (instance, mountpoint) topsrv_filesystem_bytes{type="total"} * 100
topsrv_load_average{interval="5m"} / on (instance) topsrv_cpu_cores
sum by (instance, status) (rate(topsrv_nginx_http_requests_total{status=~"5.."}[5m]))
topsrv_pg_connections / on (instance) topsrv_pg_max_connections * 100
topsrv_pg_replication_lag_seconds{stage="replay"}
(topsrv_ssl_certificate_expiry_seconds - time()) / 86400
100 * rate(topsrv_netstat_tcp_retransmits_total[5m]) / rate(topsrv_netstat_tcp_out_segs_total[5m])
```

## Логи nginx: `botlog` и `weblog`

`botlog.search` — только распознанные боты, хранение долгое. `weblog.*` — **весь**
трафик сайтов, включая живых людей, и всего **72 часа**: окно шире — ошибка
`period exceeds 72h0m0s`, без `from`/`to` — последние сутки.

`weblog.search` отдаёт сырые события (25 полей: `remoteAddr`, `country`, `asn`,
`networkType`, `userAgent`, `visitorId`, `ipFeeds`, `cacheStatus`, `requestId`),
остальные четыре считают по той же выборке. Один и тот же `filter` у всех: 28 полей,
и почти все — **массивы** (`host`, `path`, `status`, `statusClass`, `country`,
`asn`, `networkType`, `botName`, `verifyState`, `visitorId`, `requestId`…), а не
строки. Особые: `remoteAddr` берёт и адрес, и CIDR (`203.0.113.0/24`); `uaMatched` и
`ipListed` — булевы; `requestTimeMinMs` — число. `statusClass`: `2xx`…`5xx` и
`dropped` (444, 499 и всё выше 599 — это сайт оборвал клиента, не сайт упал).

- `weblog.top` — `groupBy` обязателен: `host`, `hostPath`, `path`, `pathTopSection`,
  `remoteAddr`, `asn`, `userAgent`, `country`, `status`, `platform`, `appVersion`,
  `platformVersion`, `visitorId`. `sortBy`: `requests`, `bytes`, `effort`, `avgMs`,
  `p95Ms`, `c4xx`, `c5xx`, `firstSeen`, `lastSeen`. Кто **дороже всего** обходится —
  это `effort`, а не `requests`.
- `weblog.networks` — по автономным системам; `sortBy: "uaPerIp"` поднимает наверх
  пулы: мало User-Agent на много адресов. `minIPs` срезает хвост из сетей, замеченных
  однажды. Ответ — массив без конверта.
- `weblog.blockImpact` — что зацепит блокировка, до того как её напишут: отдельно
  адрес, User-Agent, оба вместе, `/24` и весь ASN. Судить по `visitorsOnlyHere` —
  посетители, которых больше неоткуда видно; `visitors` завышает потери. Фильтров у
  метода нет намеренно: блок действует на весь трафик проекта. Сам блок ringsrv не
  пишет — таргет только читает.
- Пагинация у `weblog.search` — **по времени**: `sortDir: "asc"`, потом двигать `from`
  на время последнего события; события с этой же секундой придут второй раз, дубли
  снимаются по `requestId`. `offset` есть (до 10000), но глубокий стоит дорого.
  Лимиты: `search` и `top` — 500 на страницу, `networks` — 200.
- **`requestId` — мост в логи сервиса:** тот же идентификатор nginx отдал бэкенду, так
  что запрос из `grafana`/Loki и событие здесь сходятся по одному ключу.
- **Нули — не ошибка запроса.** На 2026-09-09 `weblog.summary` за все 72 часа отдавал
  нули: скоуп `weblogs:read` у токена есть, методы отвечают, а сбор веб-логов на
  проекте ещё не включён. Признак — `events: 0` при живом `botlog.search`.

## Время

`start` и `end` у `metric.queryRange` — unix-секунды **числом**; `now`, `now-1h`,
`now-24h`, `now-7d`, написанные строкой, разворачивает ringsrv. Другой формат строки
topsrv не примет. Окно логов лежит иначе: `filter.from` и `filter.to` у `botlog.search`
и `weblog.*` — строки RFC3339, и `now-6h` там тоже разворачивается, в строку.
Мгновенный `metric.query` времени не берёт вовсе: прошлое — через `offset 1h` или
`max_over_time(...[1h])` внутри PromQL.

## Грабли

- **Ошибка приходит с HTTP 200.** Признак — `error` с кодом: `403 insufficient_scope`,
  `400 bad_query` (в `data` — сообщение парсера PromQL), `400 invalid_range` (нет `start`
  или `end`, или `end` не позже `start`), `400 too_many_points`, `400 empty_query`,
  `404 host_not_found`, `429 rate_limited`. У `weblog.*` свои: `400 invalid groupBy: <что
  написали>`, `400 ip or userAgent is required`, `400 period exceeds 72h0m0s`. Неизвестный
  метод — `-32601 Method not found`.
- **Не больше 11000 точек на ряд.** Окно, делённое на `step`, выше этого — `too_many_points`:
  неделя по `1m` ещё проходит, восемь дней — уже нет. Без `step` сервер подбирает шаг
  сам: за час `1m`, за сутки `2m`, за неделю около `11m`. Разумный свой шаг — период на
  200–500 точек.
- **`id` обязателен.** Запрос без `id` — уведомление: сервер выполнит его и ответит
  пустым телом.
- **Батч — не больше 10.** Одиннадцатый элемент — `HTTP 403` текстом «Batch size
  exceeded», без JSON.
- **Что горит сейчас** видно в `alert.rules`: пока событие открыто, строка правила несёт
  `currentState` и `activeEventId`, у тихого правила этих ключей нет. Само событие с
  `value` и `startedAt` — `alert.list` по `state=firing`; `value` — измеренное значение
  против `threshold` правила по `ruleCode`.
- **`inventory.*`, `botlog.*` и `weblog.*`** есть, только пока за ними работает ClickHouse;
  у токена на них отдельные скоупы `inventory:read`, `botlogs:read` и `weblogs:read`.
- **`botlog.search`:** без `from`/`to` — последние сутки; `statusClass` — `2xx`…`5xx`,
  `verifyState` — `verified`, `spoofed`, `unverified`; `host` — заголовок Host запроса,
  `agentHostname` — машина, на которой стоит nginx. `host`, `statusClass`, `verifyState`
  — **массивы строк**: `"host":"example.com"` даёт `-32602`. Фильтров по URI и боту нет:
  незнакомые поля (`uri`, `botFamily`) молча игнорируются, ответ выглядит как «всё
  подряд» — сужайте окно и отбирайте по `uri`/`botName` через `jq`. Ответ —
  `{events, total}`, время события в поле `time`; `limit` до 500, дальше `offset`.
- **Лимиты:** 60 запросов в минуту на токен, burst 20. На `rate_limited` подождите
  минуту, а не повторяйте сразу.
