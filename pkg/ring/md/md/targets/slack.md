---
name: slack
description: Slack через api_call — поиск сообщений и чтение тредов в публичных каналах; тред по ссылке, синтаксис query, ошибки при HTTP 200, ts, thread_ts и permalink.
---

# Таргет `slack` — обсуждения

Только чтение, только `GET`. Токен один на всех, и скоупы у него только на публичное:
приватные каналы и личные сообщения не видны ни поиску, ни чтению — запрос к ним
отвечает `missing_scope`. Все, у кого есть этот таргет, видят одно и то же.

`DefaultJQ` поднимает ошибку Slack наверх и оставляет от ответа читаемое: у поиска —
`matches` с полями `channel` (id), `name`, `user` (имя автора, у бота — `bot`), `ts`,
`thread_ts`, `reply_count`, `text`, `permalink`, а с `include_context_messages=true` ещё
`context` — соседние сообщения `before` и `after`; у треда и истории — `user` (id), `ts`,
`thread_ts`, `reply_count`, `text`. Разметка `blocks`, реакции и файлы отбрасываются.
Нужны — передайте свой `jq`.

**На этом таргете включена редакция PII** в режиме `warn`: тело не меняется, список
сработавших правил приезжает в поле `redacted`.

## Разрешённые пути

| Путь | Зачем |
|---|---|
| `/api/assistant.search.context` | поиск сообщений, основной вход; находит и ответы внутри тредов |
| `/api/conversations.replies` | тред целиком по `channel` и `ts` |
| `/api/conversations.history` | последние сообщения канала |
| `/api/conversations.list` | каналы: id по имени |
| `/api/conversations.info` | один канал: тема и назначение; число участников — только с `include_num_members=true` |
| `/api/users.info` | кто такой `U…` из текста |

Параметры запроса — только `query`, `sort`, `sort_dir`, `after`, `before`, `highlight`,
`include_bots`, `include_context_messages`, `disable_semantic_search`, `team_id`, `channel`,
`ts`, `limit`, `cursor`, `oldest`, `latest`, `inclusive`, `types`, `exclude_archived`,
`include_num_members`, `include_all_metadata`, `include_locale`, `user`; `limit` не больше
1000, как у истории Slack, а у поиска Slack сам режет его до 20. Другое имя или больше —
`QueryParamNotAllowed` со списком, а не `invalid_arguments` при HTTP 200.

## Примеры

```
/api/assistant.search.context?query=deploy+rollback&after=now-7d&sort=timestamp&limit=20
/api/assistant.search.context?query=in:%23incidents+is:thread+deploy&limit=20
/api/assistant.search.context?query=%22connection+refused%22&include_context_messages=true
/api/assistant.search.context?query=pipeline+failed+on:2026-09-01&include_bots=true
/api/conversations.replies?channel=C0123456789&ts=1725000000.123456&limit=200
/api/conversations.history?channel=C0123456789&oldest=now-24h&limit=100
/api/conversations.list?types=public_channel&exclude_archived=true&limit=200
/api/conversations.info?channel=C0123456789&include_num_members=true
/api/users.info?user=U0123456789
```

## Как прочитать тред

**По ссылке.** `https://<workspace>.slack.com/archives/C0123456789/p1725000000123456`:

1. `channel` — сегмент после `/archives/`, здесь `C0123456789`.
2. `ts` — цифры после `p` с точкой перед последними шестью: `p1725000000123456` →
   `1725000000.123456`.
3. Если в ссылке есть `?thread_ts=1725000000.000001`, это ответ внутри треда, и `ts` для
   `conversations.replies` — значение `thread_ts`, а не то, что после `p`.
4. `/api/conversations.replies?channel=C0123456789&ts=1725000000.000001` — первым идёт
   корень, дальше ответы по порядку. `has_more: true` — продолжайте с `cursor=<next_cursor>`.

**Из поиска.** `thread_ts` в результате есть — сообщение лежит в треде (корень или ответ):
`conversations.replies` с `channel` и `ts=<thread_ts>`. `thread_ts` нет — сообщение вне
треда; что было рядом, покажет `include_context_messages=true`.

## Грабли

- **Ошибка приходит с HTTP 200.** Признак — `ok: false` и `error`: `missing_scope`,
  `channel_not_found`, `not_in_channel`, `ratelimited`, `invalid_auth`. `DefaultJQ`
  оставляет от такого ответа только их.
- **`query` — это синтаксис поиска Slack, а не текст.** `in:%23channel`, `from:@user`,
  `after:2026-09-01`, `before:`, `on:`, `during:september`, `is:thread`, `has:link`,
  фраза в кавычках (`%22…%22`). Пробел кодируйте как `+`, решётку как `%23`, кавычку как
  `%22`. Канал задаётся только так, через `in:` — отдельного параметра для него у поиска нет.
- **Период — параметрами или внутри `query`.** `after` и `before` принимают `now-7d`,
  сервер превращает их в unix-секунды; `after:2026-09-01` внутри `query` работает тоже.
  У истории и тредов то же самое делают `oldest` и `latest`.
- **Поиск бывает смысловым.** Если план Slack включает AI-поиск, по умолчанию находится и
  близкое по смыслу; нужны точные слова — `disable_semantic_search=true`.
- **Боты попадают в выдачу непредсказуемо.** Алерты и сообщения CI ищите с
  `include_bots=true`; автор у них — `bot`.
- **`ts` — строка с точкой**, `1725000000.123456`, и одновременно идентификатор сообщения.
  Передавайте как есть, не округляйте и не превращайте в число.
- **Пагинация одна — `cursor`**, но страницы разные: у поиска не больше 20 результатов, у
  остальных `limit` до 1000. Следующая страница — `cursor=<next_cursor>`.
- **Упоминания в тексте остаются идентификаторами:** `<@U0123456789>` — пользователь,
  `<#C0123456789|name>` — канал, `<https://…|текст>` — ссылка. В треде и истории `user` —
  тоже id; имя по `U…` даёт `users.info`. Автор результата поиска уже приходит именем.
- **`missing_scope`, `channel_not_found` и `not_in_channel`** для канала, который точно
  есть, означают приватный канал или чужой workspace: токен его не видит, и это ожидаемо.
  Публичные каналы видны все, вступать в них не нужно.
- **Глубина истории — свойство плана Slack**, не таргета: на бесплатном плане поиск
  не видит дальше 90 дней.
- **Лимиты:** поиск — около 10 запросов в минуту на весь таргет, потому что токен один на
  всех, и каждая страница — отдельный запрос; треды и история — около 50. На `ratelimited`
  подождите минуту, а не повторяйте сразу.
