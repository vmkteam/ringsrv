---
name: prom
description: Prometheus через api_call — разрешённые пути, PromQL-примеры, форматы времени и типичные ошибки.
---

# Таргет `prom` — Prometheus

Метрики сервисов. Только чтение, только `GET`. `DefaultJQ = .data.result` — то есть
по умолчанию из ответа остаётся сразу массив рядов, без обёртки `{"status":"success","data":{...}}`.

## Разрешённые пути

| Путь | Зачем |
|---|---|
| `/api/v1/query` | мгновенное значение (`instant query`) |
| `/api/v1/query_range` | ряд за период — для графиков и трендов |
| `/api/v1/labels` | список имён меток |
| `/api/v1/series` | серии по матчеру |
| `/api/v1/label/<name>/values` | значения одной метки — так узнают список job, instance, status |
| `/api/v1/status/buildinfo` | версия Prometheus — проверка, что таргет вообще отвечает |

Всё остальное (`/api/v1/admin/*`, `/-/reload`, `/api/v1/write`) не проходит: пути нет
в allowlist, запрос вернёт `PathNotAllowed` со списком паттернов.

Параметры запроса — только `query`, `time`, `timeout`, `start`, `end`, `step`, `limit`,
`match[]`, `lookback_delta`, и `step` не короче `15s`: меньший шаг дублирует точки, а не
добавляет их. Другое имя или меньший шаг — `QueryParamNotAllowed` со списком.

## Время

`start`, `end` и `time` разворачиваются сервером: можно писать `now`, `now-1h`, `now-24h`,
`now-7d` — придёт unix-время в секундах, как ждёт Prometheus. Считать даты шеллом
не нужно.

## Примеры

Query-параметры передаются прямо в `path`:

```
api_call(calls: [{target: "prom", method: "GET",
                  path: "/api/v1/query?query=sum(rate(app_http_requests_total{job=\"apisrv\",code=~\"5..\"}[5m]))"}])
```

Ещё пути, которые чаще всего нужны:

```
/api/v1/query?query=up{job="apisrv"}
/api/v1/query_range?query=histogram_quantile(0.99,sum by (le) (rate(app_http_request_duration_seconds_bucket{job="apisrv"}[5m])))&start=now-6h&end=now&step=60s
/api/v1/label/job/values
```

## Грабли

- **`step` обязателен** для `query_range`, иначе Prometheus ответит 400. Разумный шаг —
  период, делённый на 200–500 точек: за 6 часов это `60s`, за неделю — `30m`.
- **Диапазон в `rate()` должен быть не меньше двух scrape-интервалов.** При обычном
  scrape-интервале в 15 с `[1m]` — минимум, а `[5m]` — рабочий дефолт.
- **Пустой ответ (`data.result == []`) — это не ошибка**, а «нет таких серий». Проверьте
  имя `job`: оно берётся из `[Repos.*].PromJob`, а не из имени сервиса.
- **Фильтр по умолчанию различает две формы ответа**: у `query` и `query_range` он отдаёт
  `.data.result`, у `labels`, `label/<name>/values`, `series` и `status/buildinfo` — `.data`
  целиком, потому что там результат лежит прямо в нём. Сырое тело со `status` — `jq: "."`.
- **Мгновенный `query` за прошлое** — это `query` + `time=now-3h`, а не `query_range`.
- Метрики appkit: `app_http_requests_total`, `app_http_request_duration_seconds`;
  у zenrpc-сервисов — `app_rpc_*`.
- Вызовы MCP-инструментов: `app_mcp_tool_calls_total{tool,outcome}` — общая серия
  рамы; разрез по таргету и по причине отказа (`bad_args`, `forbidden`, `timeout`) —
  только в `app_mcp_tool_target_calls_total{tool,target,outcome}`.
