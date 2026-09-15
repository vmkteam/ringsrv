---
name: gitlab
description: GitLab через api_call — файлы на конкретном SHA, сравнение, коммиты, MR, пайплайны и логи джоб; кодирование путей.
---

# Таргет `gitlab` — код, MR и пайплайны

Только чтение (`read_api`): write-профиля в каталогах нет, создать MR или комментарий отсюда нельзя.
Инстанс может быть общим для контуров (описание профиля скажет «общий инстанс»):
какой контур перед вами, говорит описание таргета, а не адрес.

## Разрешённые пути

| Путь | Зачем |
|---|---|
| `/api/v4/version` | жив ли хост и принят ли токен — единственный путь без id проекта |
| `/api/v4/projects/<id>` | проект: имя, дефолтная ветка |
| `/api/v4/projects/<id>/repository/files/<path>/raw?ref=<sha>` | файл на конкретном коммите |
| `/api/v4/projects/<id>/repository/tree?ref=<sha>&path=<dir>` | что лежит в каталоге |
| `/api/v4/projects/<id>/repository/compare?from=<sha>&to=<sha>` | что изменилось между релизами |
| `/api/v4/projects/<id>/repository/commits` | история, в том числе по пути |
| `/api/v4/projects/<id>/repository/commits/<sha>/diff` | diff одного коммита |
| `/api/v4/projects/<id>/repository/branches` | ветки; одна ветка — `/repository/branches/<branch>` |
| `/api/v4/projects/<id>/repository/tags` | теги |
| `/api/v4/projects/<id>/merge_requests?state=opened` | список MR: `state`, `target_branch`, `author_username` |
| `/api/v4/projects/<id>/merge_requests/<iid>` | один MR: автор, ветки, статус, `sha` |
| `/api/v4/projects/<id>/merge_requests/<iid>/changes` | diff MR целиком |
| `/api/v4/projects/<id>/merge_requests/<iid>/notes` | обсуждение MR, в том числе артефакты `/solve` |
| `/api/v4/projects/<id>/merge_requests/<iid>/commits` | коммиты MR |
| `/api/v4/projects/<id>/merge_requests/<iid>/pipelines` | пайплайны MR |
| `/api/v4/projects/<id>/pipelines` | пайплайны и их статусы: `ref`, `status`, `sha` |
| `/api/v4/projects/<id>/pipelines/<pipeline>/jobs` | джобы пайплайна: какая упала |
| `/api/v4/projects/<id>/jobs/<job_id>/trace` | лог джобы, текст |
| `/api/v4/projects/<id>/search?scope=blobs&search=text` | поиск по проекту |

## Примеры

```
/api/v4/projects/42/repository/files/internal%2Frpc%2Forder.go/raw?ref=abc1234
/api/v4/projects/42/repository/compare?from=abc1234&to=def5678
/api/v4/projects/42/repository/commits?ref_name=abc1234&path=internal/rpc&per_page=20
/api/v4/projects/42/merge_requests?state=merged&target_branch=devel&per_page=10
/api/v4/projects/42/merge_requests/1012/changes
/api/v4/projects/42/pipelines?ref=devel&status=failed&per_page=5
/api/v4/projects/42/jobs/76500/trace
```

## Грабли

- **Путь к файлу кодируется целиком**: `internal/rpc/order.go` → `internal%2Frpc%2Forder.go`.
  Без этого — 404, который читается как «файла нет». Имя ветки со слэшем — так же:
  `feature/x` → `feature%2Fx`.
- **`ref` обязателен и должен быть SHA деплоя**, а не веткой. Смотреть `master` вместо
  задеплоенного коммита — классическая ошибка расследования.
- **`id` проекта числовой**, берётся из `repo_map` (`GitLabProject`). Путь вида
  `backend%2Fapisrv` тоже работает, но в каталоге хранится число.
- **Все пути, кроме `/api/v4/version`, требуют id проекта**, и каждый начинается
  с префикса `/api/v4`; `/projects?per_page=1` — это `PathNotAllowed`, а не «нет
  доступа». Живость хоста и валидность токена проверяются `/api/v4/version`.
- **MR адресуется по `iid`** — номеру внутри проекта, тому, что в URL и в `!1012`, —
  а не по глобальному `id` из списка.
- **`compare` и `changes` на большом диапазоне возвращают мегабайты.** Сузьте диапазон
  или добавьте `jq` по нужным полям; у `changes` нет `per_page`, для большого MR
  смотрите `commits` и diff отдельных коммитов.
- **`trace` — текст, не JSON**: `jq` к нему неприменим, а лог долгой джобы больше
  `max_bytes` и будет усечён. Сначала `pipelines/<pipeline>/jobs`, чтобы взять
  именно упавшую джобу.
- **Веер по проектам — один вызов.** Коммиты за период по двадцати репозиториям,
  списки MR, комментарии к найденным MR: это один `api_call` со списком `calls`
  (до 20), а не двадцать вызовов подряд. Ответы приходят в `results[]` в порядке
  отправки, упавший элемент не отменяет остальные, а `max_bytes` — бюджет на весь
  вызов: если ответы широкие, сужайте `jq` или делите список.
- **Поиск индексирует только дефолтную ветку**, поэтому на задеплоенном SHA он
  бесполезен — для этого есть `code_search` по зеркалу.
