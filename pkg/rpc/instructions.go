package rpc

import "fmt"

// The instructions are the voice of this service: what the model must know
// before its first call and nothing it must not. They stay here, not in the
// library — mcpkit takes them as InitDeps.Instructions and has no opinion
// about what they say.
//
// The %s is the contour. It comes from the catalogue the instance was started
// with and never from an argument (D25), so the model cannot ask about prod
// from a dev instance by saying so.
const initInstructions = `ringsrv — MCP-прокси к наблюдаемости и коду vmkteam: Grafana, Prometheus, Sentry, YouTrack, GitLab, Nomad, Slack, topsrv плюс сам код в git.

Окружение инстанса — %s. Оно задано конфигом и не меняется аргументом вызова.

⚠️ Инструменты принимают СПИСОК и отвечают results[] в порядке отправки: api_call(calls), db_query(queries), db_introspect(tables), help(names), repo_map(repos), code_search(repos), code_read(windows). Клиент отправляет вызовы по одному и ждёт каждый ответ, поэтому пять вопросов одним вызовом быстрее пяти вызовов. Планируй шаг целиком: всё, что не зависит от предыдущего ответа, уходит вместе. Одиночный вопрос — список из одного.

Два вида вопросов, два набора инструментов:
  • наблюдаемость — api_call(calls: [{target, method, path, …}]). Каталог доступных таргетов в его описании; он собран под ваши группы, поэтому там ровно то, что вам разрешено. Веер по списку id (коммиты по проектам, MR, комментарии) — один вызов со списком; упавший элемент не отменяет остальные;
  • код — code_read, code_search, code_history, code_refs, blast_radius, why. Все читают код на конкретном коммите: ref обязателен, потому что ветка — не то, что задеплоено. Как сервис называется в git, Sentry, Nomad и Prometheus, говорит repo_map. Один и тот же запрос по нескольким репозиториям — это repos в одном code_search, а не вызов на репозиторий.
  • данные — db_query и db_introspect, если роли выданы базы. Это readonly SQL к базе из каталога. Перед первым SQL вызови db_introspect и перечисли в нём все нужные таблицы сразу. В db_query список уместен для независимых агрегатов; при первой ошибке остальные запросы не выполняются.

Шпаргалки перед первым вызовом — инструмент help: help(names: [...]) с именами таргетов, code для инструментов по коду или db для баз; help() без аргумента — список. Нужны три шпаргалки — запрашивай три сразу. Те же тексты лежат ресурсами ringsrv://targets/<name>.md, ringsrv://tools/code.md, ringsrv://tools/db.md.

Готовые сценарии — prompts/list и prompts/get: расследование инцидента, дайджест ошибок, триаж, объяснение кода.

Правила, которые экономят попытки:
  • path относительный, начинается с "/"; абсолютные URL, ".." и свои заголовки отклоняются;
  • время в query-параметрах можно писать как now-1h / now-24h / now-7d — сервер развернёт в формат таргета;
  • ответ фильтруется jq: у большинства таргетов есть DefaultJQ, свой jq передаётся аргументом;
  • ошибка — это документация: TargetUnknown отдаёт список таргетов, PathNotAllowed — паттерны, JQFailed — ключи ответа, AmbiguousSymbol — файлы-кандидаты;
  • пустой ответ — это ответ: «такой строки в коммите нет», «релиз не трогал упавший код». Перебирать запросы не надо;
  • запись (issue, комментарий, MR) требует аргумента intent и доступна только ролям с этим правом;
  • у работы часовой бюджет: api_call, db_query и db_introspect отвечают полем budget — сколько потрачено, сколько осталось и когда сбросится. Когда осталось мало — сужай запросы через jq и лимит строк, а не повторяй их. Отказ -32010 по бюджету — не падение сервера: в нём сказано, сколько ждать, а help и repo_map работают и тогда.
`

// instructions fills the contour into the sheet above.
func instructions(env string) string { return fmt.Sprintf(initInstructions, env) }
