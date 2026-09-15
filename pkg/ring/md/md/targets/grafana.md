---
name: grafana
description: Grafana через api_call — поиск дашбордов, datasources, аннотации и единственный разрешённый POST.
---

# Таргет `grafana` — дашборды и аннотации

Чтение плюс один POST: `/api/ds/query`. Он семантически чтение (выполнить запрос к
datasource), поэтому профиль помечен `ReadOnlyPost`, а не `Write`. POST разрешён
**только** на этот путь: `POST /api/annotations` или `POST /api/datasources` —
это запись, и она отклоняется с `PathNotAllowed`.

## Разрешённые пути

| Путь | Метод | Зачем |
|---|---|---|
| `/api/search` | GET | найти дашборд по имени или тегу |
| `/api/dashboards/uid/<uid>` | GET | JSON дашборда: панели и их запросы |
| `/api/datasources` | GET | какие источники подключены и их uid |
| `/api/datasources/uid/<uid>` | GET | один datasource: тип, url, uid |
| `/api/datasources/uid/<uid>/resources/api/v1/labels` | GET | имена меток источника; так же `series`, `metadata`, `label/<name>/values` |
| `/api/annotations` | GET | аннотации: деплои, инциденты |
| `/api/health` | GET | жив ли инстанс |
| `/api/ds/query` | POST | выполнить запрос к datasource |

Всё остальное — админка, пользователи, создание дашбордов и аннотаций, а также
datasource-proxy (`/api/datasources/proxy/…`) — не проходит. Прокси закрыт
намеренно: он обходит allowlist самого источника, а для Prometheus и Loki есть
свои таргеты.

## Примеры

```
/api/search?query=apisrv&limit=10
/api/dashboards/uid/abc123def
/api/annotations?from=now-24h&to=now&limit=50
```

Тело для `/api/ds/query`:

```json
{"queries":[{"refId":"A","datasource":{"uid":"prom-uid"},"expr":"up{job=\"apisrv\"}"}],"from":"now-1h","to":"now"}
```

## Грабли

- **Дашборд полезнее как источник запросов, чем как картинка.** В JSON панели лежит
  готовый PromQL, проверенный людьми, — его дешевле взять оттуда, чем сочинять.
- **`uid` датасорса нужен для `/api/ds/query`** и берётся из `/api/datasources`.
  Имя вместо uid — 400.
- **Прямой запрос в Prometheus обычно проще**: у нас есть таргет `prom`, и он отдаёт
  чистые ряды без обёртки Grafana. `/api/ds/query` нужен, когда важно повторить
  запрос панели ровно так, как его видит дежурный.
- **Поиск по `/api/search` — по названию и тегам**, не по содержимому панелей.

## Loki через Grafana

Если у Loki нет своего таргета, логи ходят через этот профиль. Datasource `loki`
ищется в `GET /api/datasources` по `type = "loki"`, дальше нужен его `uid`; если
он известен заранее, он записан в описании профиля.

| Путь | Метод | Что даёт |
|------|-------|----------|
| `/api/datasources/uid/<uid>/resources/labels` | GET | имена меток; у Loki путь без `api/v1/`, с ним 404 |
| `/api/datasources/uid/<uid>/resources/label/<name>/values` | GET | значения метки |
| `/api/ds/query` | POST | сами строки логов |

Имена меток зависят от установки: возьмите их из описания профиля или из
`/api/datasources/uid/<uid>/resources/labels`. Там, где логи собирает агент
оркестратора, метки `job` может не быть вовсе — `{job="apisrv"}` вернёт пустоту, а нужна метка вроде
`{_job_name="apisrv"}`. У dispatch- и periodic-джоб значение идёт с суффиксом
(`<job>/dispatch-…`), поэтому берите `=~"<job>.*"` или метку задачи.

Тело для `/api/ds/query`:

```json
{"queries":[{"refId":"A","datasource":{"uid":"<uid>","type":"loki"},"expr":"{<label>=\"<job>\"} |= \"error\"","queryType":"range","maxLines":50}],"from":"now-24h","to":"now"}
```

Ответ — фрейм с колонками `labels, Time, Line, tsNs, labelTypes, id`, значения
лежат по колонкам. jq, чтобы получить строки со временем:

```
[.results.A.frames[0].data.values | transpose[] | {t: .[1], line: .[2]}]
```

Пустой `values` — не ошибка, а «ничего не нашлось»: проверьте селектор через
`resources/label/_job_name/values`, прежде чем расширять окно времени.
