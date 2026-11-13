---
name: mattermost
description: Mattermost через api_call — поиск сообщений, тред по ссылке, история канала, каналы, люди и файлы вложений (текст, картинки, xlsx, zip) в публичных каналах; синтаксис поиска, id и ссылки, время в миллисекундах.
---

# Таргет `mattermost` — обсуждения

Только чтение и только **публичные каналы**: поиск, тред, история канала, каналы команды, люди,
файлы вложений. Приватные каналы, личку и групповые чаты сервер отсекает сам. Путь к такому
каналу или сообщению — отказ `NotPublic` ещё до вызова, а в выдаче поиска таких сообщений просто
нет.

**Что приходит.** `default_jq` сворачивает списки сообщений в `posts` — в порядке Mattermost, от
новых к старым, с полями `id`, `channel_id`, `root_id` (пусто — корень треда), `user_id`,
`override_username` (имя, под которым писал бот-мост), `create_at` и `edit_at` (миллисекунды
Unix), `message`, `reply_count`, `files` (`id`, `name`, `extension`, `size`) и `attachments`
бота (`pretext`, `title`, `text`, `fields`). Пустые поля выкидываются. У каналов — `id`, `name`,
`display_name`, `type`, `team_id`, `purpose`, `header`; у людей — `id`, `username`,
`first_name`, `last_name`, `nickname`, `position`, `is_bot`. Нужно другое — передайте свой `jq`.

## Пути

| Путь | Что |
|---|---|
| `/api/v4/posts/search` | POST, поиск по всем командам |
| `/api/v4/teams/<team_id>/posts/search` | POST, поиск в одной команде |
| `/api/v4/posts/<post_id>/thread` | тред целиком: корень и ответы |
| `/api/v4/posts/<post_id>` | одно сообщение |
| `/api/v4/channels/<channel_id>/posts` | история канала по страницам |
| `/api/v4/channels/<channel_id>` | канал: имя, тема, назначение |
| `/api/v4/teams/name/<team>/channels/name/<channel>` | канал по именам из ссылки |
| `/api/v4/teams/<team_id>/channels` | публичные каналы команды по страницам |
| `/api/v4/teams/<team_id>/channels/search` | POST `{"term":"deploy"}` — каналы по части имени |
| `/api/v4/teams/name/<team>` | команда по имени из ссылки: её `id` |
| `/api/v4/users/<user_id>` | кто такой `user_id` |
| `/api/v4/users/username/<username>` | человек по `@логину` |
| `/api/v4/users/ids` | POST `["id1","id2"]` — люди по списку id |
| `/api/v4/teams/<team_id>/files/search` | POST — файлы по имени |
| `/files/download` | содержимое файла по `file` — отвечает сам ringsrv, см. `help(["files"])` |

Параметры запроса — только `page`, `per_page`, `since`, `before`, `after`, `include_deleted`,
`file`, `offset`, `limit`, `grep`, `sheet` и `entry`; `per_page` не больше 200, `limit` — не
больше 1000. Текст поиска передаётся телом запроса, не параметром.

## Примеры

```
/api/v4/posts/kx3f9a8b7c6d5e4f3g2h1j0k9m/thread
/api/v4/channels/4tq8wz1s3b9xpd6m7nqa2kcfhy/posts?per_page=50
/api/v4/channels/4tq8wz1s3b9xpd6m7nqa2kcfhy/posts?since=1762300800000
/api/v4/teams/name/acme/channels/name/town-square
/api/v4/users/username/ann.smith
/files/download?file=9xk2m4n6p8q1r3s5t7u9w0y2za&grep=ERR-1042
```

Поиск — `POST /api/v4/posts/search` с телом:

```json
{"terms": "релиз in:deploys after:2026-11-01", "is_or_search": false, "page": 0, "per_page": 20, "time_zone_offset": 10800}
```

## Синтаксис поиска

Слова через пробел — все сразу (`is_or_search: true` — любое). `"точная фраза"`, `-слово`
исключает, `заказ*` — по началу слова, `#тег`. Фильтры: `from:ann.smith`, `in:town-square`
(имя канала, не отображаемое), `after:2026-11-01`, `before:2026-11-08`, `on:2026-11-05`.
`time_zone_offset` — смещение в секундах, чтобы даты фильтров читались по вашему времени.

## Как прочитать тред по ссылке

1. Ссылка на сообщение — `https://<host>/<team>/pl/<post_id>`: `post_id` — последний сегмент.
2. `/api/v4/posts/<post_id>/thread` — тред целиком, какое бы сообщение треда ни было в ссылке.
3. Ссылка на канал — `https://<host>/<team>/channels/<channel>`: канал по именам —
   `/api/v4/teams/name/<team>/channels/name/<channel>`, дальше история по его `id`.

**Из поиска.** `root_id` есть — сообщение в треде, тред открывается по `root_id`; пусто — это
корень или сообщение вне треда, тред открывается по его `id`.

## Как прочитать файл

В `files` у сообщения — `id`, `name`, `extension` и `size` вложения. `/files/download?file=<id>`
отдаёт содержимое: текст строками, картинку, лист xlsx (`sheet`), оглавление или файл zip
(`entry`). Только файлы публичных каналов, не больше 10 МБ. Подробно, с отказами —
`help(["files"])`.

## Грабли

- **Время — миллисекунды Unix**: `create_at`, `since`. `since` отдаёт всё изменённое после
  метки, правки и удаления тоже, и без страниц.
- **Что попадает в поиск, решает сервер Mattermost.** Обычно он ищет только по каналам, где
  состоит учётная запись таргета; тред, история и файлы публичного канала читаются и без этого.
  Поиск пуст, а канал известен — читайте историю канала.
- **Морфология поиска зависит от настроек базы Mattermost.** Слово не нашлось в другой форме —
  ищите по началу: `заказ*`.
- **`/api/v4/teams` может показать не все команды**: обычной учётке Mattermost отдаёт открытые.
  Команду из ссылки берите по имени — `/api/v4/teams/name/<team>`.
- **Ошибка — HTTP-статусом**: 400 — неверный id (26 символов `a–z0–9`), 403 — нет доступа,
  404 — нет такого.
- **`NotPublic`** — канал или сообщение не в публичном канале, или путь, который сервер не умеет
  проверить. Это ожидаемо, повторять не нужно.
