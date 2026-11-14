package rpc

import (
	"fmt"
	"strings"

	"github.com/vmkteam/ringsrv/pkg/ring/kb"

	"github.com/vmkteam/mcpkit/mcp"
)

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

Шпаргалки перед первым вызовом — инструмент help: help(names: [...]) с именами таргетов, code для инструментов по коду, db для баз или files для чтения файлов; help() без аргумента — список. Нужны три шпаргалки — запрашивай три сразу. Те же тексты лежат ресурсами ringsrv://targets/<name>.md, ringsrv://tools/code.md, ringsrv://tools/db.md, ringsrv://tools/files.md.

Готовые сценарии — prompts/list и prompts/get: расследование инцидента, дайджест ошибок, триаж, объяснение кода.

Правила, которые экономят попытки:
  • path относительный, начинается с "/"; абсолютные URL, ".." и свои заголовки отклоняются;
  • время в query-параметрах можно писать как now-1h / now-24h / now-7d — сервер развернёт в формат таргета;
  • ответ фильтруется jq: у большинства таргетов есть DefaultJQ, свой jq передаётся аргументом;
  • ошибка — это документация: TargetUnknown отдаёт список таргетов, PathNotAllowed — паттерны, JQFailed — ключи ответа, AmbiguousSymbol — файлы-кандидаты;
  • пустой ответ — это ответ: «такой строки в коммите нет», «релиз не трогал упавший код». Перебирать запросы не надо;
  • запись (issue, комментарий, MR) требует аргумента intent и доступна только ролям с этим правом.
`

// budgetInstructions is added only on an instance with an hourly budget: told
// about a field that never comes, the model waits for it or goes looking. The
// %d is mcp.CodeRateLimited, the code a refusal by the budget arrives with.
const budgetInstructions = `
У работы часовой бюджет. Ответ каждого инструмента, который его тратит, и help несут поле budget: сколько потрачено, сколько осталось и когда сбросится. Когда осталось мало — сужай запросы (jq, лимит строк, окна поменьше), а не повторяй их. Отказ %d по бюджету — не падение сервера: в нём сказано, сколько ждать, а help, repo_map, ресурсы и промпты работают и тогда.
`

// coreInstructions is what every instance says whatever its knowledge pack
// says: the contour, the lists, help, and how to read a refusal. It stands
// alone only in front of a pack that replaces the sheet above — an instance
// without a pack keeps that sheet word for word, and the sheet says all of this
// already. The %s is the contour, as above.
const coreInstructions = `Окружение инстанса — %s. Оно задано конфигом и не меняется аргументом вызова.

⚠️ Инструменты принимают СПИСОК и отвечают results[] в порядке отправки. Клиент отправляет вызовы по одному и ждёт каждый ответ, поэтому пять вопросов одним вызовом быстрее пяти вызовов. Планируй шаг целиком: всё, что не зависит от предыдущего ответа, уходит вместе. Одиночный вопрос — список из одного.

Шпаргалки и база знаний — инструмент help: help(names: [...]) с именами из help() без аргумента; имя, которое кончается на «/», — список раздела. Нужны три — запрашивай три сразу. Готовые сценарии — prompts/list и prompts/get.

Ошибка — это документация: отказ называет, что доступно. Пустой ответ — это ответ, перебирать запросы не надо.
`

// envPlaceholder is the one substitution a pack's instructions get: the
// contour, from the catalogue and never from an argument (D25).
const envPlaceholder = "{{env}}"

// instructions fills the contour into the sheet, puts the pack's voice where
// its mode says, and adds the budget when the instance has one. Without a pack
// the text is what it was before packs existed.
func instructions(env string, budget bool, pack *kb.Pack) string {
	text := fmt.Sprintf(initInstructions, env)
	if pack != nil {
		if ins, ok := pack.Instructions(); ok {
			body := strings.ReplaceAll(ins.Body, envPlaceholder, env)
			switch ins.Mode {
			case kb.ModeReplace:
				text = body + "\n\n" + fmt.Sprintf(coreInstructions, env)
			case kb.ModeAppend:
				text += "\n" + body + "\n"
			}
		}
	}
	if budget {
		text += fmt.Sprintf(budgetInstructions, mcp.CodeRateLimited)
	}
	return text
}
