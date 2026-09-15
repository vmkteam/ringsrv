---
name: youtrack
description: YouTrack через api_call — задачи, база знаний и проекты, только чтение; синтаксис query и обязательный fields.
---

# Таргет `youtrack` — задачи и база знаний (чтение)

Только чтение: write-профиля в каталогах нет, завести задачу или комментарий отсюда нельзя.

## Разрешённые пути

| Путь | Зачем |
|---|---|
| `/api/issues` | поиск задач |
| `/api/issues/<ID>` | одна задача, например `/api/issues/ABC-123` |
| `/api/articles` | список статей базы знаний |
| `/api/articles/<AID>` | одна статья, например `/api/articles/ABC-A-1` |
| `/api/admin/projects` | список проектов и их короткие имена |

Параметры запроса — только `fields`, `query`, `$top`, `$skip`, `customFields`, и `$top` не
больше 200: дальше — `$skip`. Другое имя или больший `$top` — `QueryParamNotAllowed` со списком.

## Обязательный `fields`

YouTrack по умолчанию возвращает почти пустой объект: без `fields` придёт только `id`
и `$type`. Перечисляйте, что нужно:

```
/api/issues/ABC-123?fields=idReadable,summary,description,created,customFields(name,value(name))
/api/issues?query=project:ABC %23Unresolved&fields=idReadable,summary,created&$top=20
/api/issues?query=ABC-123&fields=idReadable,summary,comments(text,created,author(login))
/api/articles/ABC-A-1?fields=idReadable,summary,content,updated,author(login)
/api/articles?fields=idReadable,summary,parentArticle(idReadable)&$top=50
```

## База знаний

Статьи живут в том же API и под тем же токеном. `content` — текст статьи в Markdown,
и без него в ответе будет только заголовок. Подпутей у статьи нет: комментарии и
дочерние статьи берутся через `fields`, как комментарии задачи.

## Грабли

- **`fields` вложенный**: `customFields(name,value(name))`. Забудете внутренние
  скобки — получите `$type` вместо значения.
- **`#` в query нужно кодировать** как `%23`: `#Unresolved`, `#Bug`.
- **`$top` вместо `limit`.** По умолчанию отдаётся немного, и это не ошибка.
- **`idReadable` — это `ABC-123`**, а `id` — внутренний `2-1234`. В отчётах и коммитах
  живёт первый; связка с кодом идёт через него (D17).
- **у статьи `idReadable` — `ABC-A-1`**, с буквой `A` между ключом проекта и номером:
  `ABC-1` и `ABC-A-1` — разные объекты в разных разделах API.
- **Комментарии приходят только по явному запросу** `comments(text,...)` и часто
  содержат то, ради чего задача и открывалась.
