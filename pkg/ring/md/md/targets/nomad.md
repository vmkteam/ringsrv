---
name: nomad
description: Nomad через api_call — джобы, аллокации, ноды; что оставляет фильтр по умолчанию и как получить остальное.
---

# Таргет `nomad` — оркестрация

Только чтение. Ни запуска, ни остановки, ни dispatch, ни scale: этих путей нет в allowlist и
не будет — валидатор каталога отдельно проверяет, что они не проходят.

## Разрешённые пути

| Путь | Зачем |
|---|---|
| `/v1/status/leader` | жив ли кластер |
| `/v1/agent/health` | здоровье агента, к которому пришёл запрос |
| `/v1/jobs` | список джоб: сервисы плюс сводка по dispatch-детям |
| `/v1/allocations` | все аллокации; `?filter=ClientStatus == "failed"` — только упавшие |
| `/v1/job/<id>` | спека джобы: образ, ресурсы, версия |
| `/v1/job/<id>/allocations` | аллокации джобы: где и в каком состоянии крутится |
| `/v1/job/<id>/deployments` | история выкатов, статус текущего |
| `/v1/job/<id>/versions` | версии спеки: что менялось между выкатами |
| `/v1/job/<id>/summary` | счётчики аллокаций по группам |
| `/v1/job/<id>/evaluations` | почему планировщик решил именно так |
| `/v1/nodes`, `/v1/node/<id>` | ноды кластера и одна подробно |
| `/v1/allocation/<id>` | одна аллокация: все события всех задач |

`<id>` dispatch- и periodic-джоб содержит слэш: `<job>/dispatch-1788330094-1ab3fcd9`.
Nomad принимает его и как есть, и как `%2F`; allowlist пропускает обе формы.

Параметры запроса — только `namespace`, `region`, `prefix`, `filter`, `meta`, `all`, `diffs`,
`resources`, `task_states`, `os`, `type`, `per_page`, `next_token`, `reverse`, `stale`; другое
имя — `QueryParamNotAllowed` со списком. `index` и `wait` не проходят: с ними запрос
становится блокирующим и висит до пяти минут, пока в кластере ничего не изменится.

**Логов аллокаций здесь нет.** Токен даёт `read`, а он не включает `read-logs` и
`read-fs`. Логи — в Loki через Grafana, см. шпаргалку `grafana`.

## Что приходит без `jq`

У профиля есть `DefaultJQ`: он узнаёт форму ответа по ключам и оставляет то, за чем
обычно ходят. Такой ответ помечен `default_jq: true`. Нужно всё — `jq: "."`.

| Ответ | Сырой | После фильтра |
|---|---|---|
| `/v1/jobs` | 350 КБ, 490 джоб | `Jobs` — джобы без родителя: `ID, Type, Status, Stop, SubmitTime, Summary`; `DispatchChildren` — по родителю: `Count, ByStatus, Failed, Last` |
| список аллокаций | 5–17 КБ | `ID, JobID, JobVersion, TaskGroup, NodeName, ClientStatus, ClientDescription, DesiredStatus, CreateTime, ModifyTime, TaskStates{State, Failed, Restarts, StartedAt, FinishedAt, LastEvent}` |
| `/v1/allocation/<id>` | 9 КБ | то же плюс `Name, NodeID, EvalID, NextAllocation` и все `Events` каждой задачи |
| `/v1/job/<id>` | 10 КБ | `ID, Name, Type, Status, Stable, Version, SubmitTime, Meta, TaskGroups[{Name, Count, Tasks[{Name, Driver, Image, CPU, MemoryMB}]}]` |
| `/v1/job/<id>/versions` | 65 КБ | `Versions[{Version, Stable, Status, SubmitTime, Meta}]` |
| остальное | | как есть |

Время (`SubmitTime`, `CreateTime`, `ModifyTime`, `Time` события) после фильтра — RFC 3339;
в сыром ответе это наносекунды.

Свой `jq` заменяет фильтр целиком, поля выбирайте сами:

```
/v1/jobs                                    jq: [.[] | select(.ParentID == "<job>") | {ID, Status, SubmitTime}]
/v1/allocations?filter=ClientStatus == "failed"
                                            jq: [.[] | {JobID, NodeName, ClientDescription, ModifyTime}]
/v1/allocation/<id>                         jq: .TaskStates | map_values(.Events[-1].DisplayMessage)
```

## Примеры

```
/v1/job/apisrv
/v1/job/apisrv/allocations
/v1/job/apisrv/deployments
/v1/job/<job>%2Fdispatch-1788330094-1ab3fcd9/allocations
```

Аллокации трёх упавших джоб — три элемента одного `calls`, а не три вызова подряд.

## Грабли

- **Имя джобы ≠ имя сервиса.** Берите `nomad_job` из `repo_map`, а не угадывайте.
- **Версия образа — в `Meta.docker_image_version` и в `TaskGroups[].Tasks[].Image`.**
  Это самый быстрый способ узнать, что реально задеплоено.
- **`Status` джобы `running` не значит, что всё хорошо**: смотрите аллокации, там
  видны рестарты и `failed`.
- **`DispatchChildren` в `/v1/jobs` — сводка, а не список.** Сами дети —
  `jq: [.[] | select(.ParentID == "<родитель>")]` или
  `/v1/allocations?filter=JobID matches "<родитель>/"`.
- **`ByStatus.dead` у batch-джоб — норма**: batch завершается в `dead`, упавшие
  считает `Failed`.
