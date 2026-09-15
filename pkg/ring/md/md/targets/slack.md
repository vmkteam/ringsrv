---
name: slack
description: Slack через api_call — поиск сообщений и чтение тредов в публичных каналах; синтаксис query, ошибки при HTTP 200, ts и permalink.
---

# Таргет `slack` — обсуждения

Только чтение, только `GET`. Токен принадлежит сервисному пользователю, который состоит
только в публичных каналах: приватные каналы и личные сообщения в поиск не попадают, а
прочитать их не даёт скоуп. Все, у кого есть этот таргет, видят одно и то же.

`DefaultJQ` поднимает ошибку Slack наверх и оставляет от ответа читаемое: у поиска —
`channel` (id), `name`, `is_private`, `user`, `ts`, `text`, `permalink`; у треда — `user`,
`ts`, `thread_ts`, `reply_count`, `text`. Разметка `blocks`, `score`, реакции и файлы
отбрасываются. Нужны — передайте свой `jq`.

**На этом таргете включена редакция PII** в режиме `warn`: тело не меняется, список
сработавших правил приезжает в поле `redacted`.

## Разрешённые пути

| Путь | Зачем |
|---|---|
| `/api/search.messages` | поиск сообщений, основной вход; находит и ответы внутри тредов |
| `/api/conversations.replies` | тред целиком по `channel` и `ts` |
| `/api/conversations.history` | последние сообщения канала |
| `/api/conversations.list` | каналы: id по имени |
| `/api/conversations.info` | один канал: тема и назначение; число участников — только с `include_num_members=true` |
| `/api/users.info` | кто такой `U…` из текста |

Параметры запроса — только `query`, `count`, `page`, `sort`, `sort_dir`, `highlight`, `team_id`,
`channel`, `ts`, `limit`, `cursor`, `oldest`, `latest`, `inclusive`, `types`, `exclude_archived`,
`include_num_members`, `include_all_metadata`, `include_locale`, `user`; `count` не больше 100, `limit` — 1000, как
у самого Slack. Другое имя или больше — `QueryParamNotAllowed` со списком, а не
`invalid_arguments` при HTTP 200.

## Примеры

```
/api/search.messages?query=apisrv+502+after:2026-09-01&count=20&sort=timestamp&sort_dir=desc
/api/search.messages?query=in:%23incidents+is:thread+deploy&count=20
/api/conversations.replies?channel=C0123456789&ts=1725000000.123456&limit=200
/api/conversations.history?channel=C0123456789&oldest=now-24h&limit=100
/api/conversations.list?types=public_channel&exclude_archived=true&limit=200
/api/conversations.info?channel=C0123456789&include_num_members=true
/api/users.info?user=U0123456789
```

## Как прочитать тред

1. Найдите сообщение поиском. `channel` в результате — id канала, он же стоит в `permalink`
   после `/archives/`. У ответа из треда в ссылке есть `thread_ts`:
   `https://<workspace>.slack.com/archives/C0123456789/p1725000000123456?thread_ts=1725000000.000001`.
2. `ts` для `conversations.replies` — это `thread_ts` из ссылки. Если `thread_ts` в ссылке
   нет, сообщение само корень треда, и `ts` — его собственный `ts`.
3. `/api/conversations.replies?channel=C0123456789&ts=1725000000.000001` — первым идёт
   корень, дальше ответы по порядку. `has_more: true` — продолжайте с `cursor=<next_cursor>`.

## Грабли

- **Ошибка приходит с HTTP 200.** Признак — `ok: false` и `error`: `missing_scope`,
  `channel_not_found`, `not_in_channel`, `ratelimited`, `invalid_auth`. `DefaultJQ`
  оставляет от такого ответа только их.
- **`query` — это синтаксис поиска Slack, а не текст.** `in:%23channel`, `from:@user`,
  `after:2026-09-01`, `before:`, `on:`, `during:september`, `is:thread`, `has:link`,
  фраза в кавычках. Пробел кодируйте как `+`, решётку как `%23`.
- **`ts` — строка с точкой**, `1725000000.123456`, и одновременно идентификатор сообщения.
  Передавайте как есть, не округляйте и не превращайте в число.
- **Пагинация разная.** У поиска `count` и `page`, не больше 100 на страницу и 100 страниц;
  у остальных `limit` и `cursor`.
- **`oldest` и `latest` принимают `now-24h`**, сервер превращает их в unix-секунды.
  У поиска периода в параметрах нет — только `after:` и `before:` внутри `query`.
- **Упоминания в тексте остаются идентификаторами:** `<@U0123456789>` — пользователь,
  `<#C0123456789|name>` — канал, `<https://…|текст>` — ссылка. Имя по `U…` даёт
  `users.info`; автор результата поиска уже приходит именем в `user`.
- **`channel_not_found` и `not_in_channel`** для канала, который точно есть, означают
  приватный канал или чужой workspace: сервисный пользователь его не видит, и это
  ожидаемо. Публичные каналы видны все, вступать в них не нужно.
- **`D…` в `channel` результата поиска — личная переписка владельца токена**, а `name`
  тогда — id собеседника. У сервисного пользователя таких результатов быть не должно;
  если есть, ему кто-то написал.
- **Глубина истории — свойство плана Slack**, не таргета: на бесплатном плане поиск
  не видит дальше 90 дней.
- **Лимиты:** поиск около 20 запросов в минуту, треды и история около 50. На
  `ratelimited` подождите минуту, а не повторяйте сразу.
