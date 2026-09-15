# ringsrv

An MCP server that puts a team's read-only infrastructure behind one allowlisted
door. A single `api_call` tool reaches the APIs named in a catalogue — Grafana,
Prometheus, Sentry, YouTrack, GitLab, Nomad, Slack — seven more tools read code
from git mirrors, and two run read-only SQL against PostgreSQL and ClickHouse
from the same catalogue. Which of them a caller sees is decided by their IdP
group.

What the catalogue buys is a surface that cannot be widened by a prompt: path
patterns are anchored, every method beyond `GET` has to be declared, writes need
an explicit intent and a role that allows them, and the samples of merge, delete
and deploy are run against every profile at load time. A target the model was
never granted is not in its `tools/list` at all.

The tools take lists: `api_call(calls)` runs up to 20 requests at once (four in
flight), `db_query(queries)` up to five in a row against one database, and
`db_introspect(tables)`, `help(names)` and `repo_map(repos)` do the same. There
is deliberately no scalar form next to them: clients send calls one at a time,
and a fan-out over ids used to cost as many round trips as it had elements — 211
calls for three questions, by the audit log. The answer is `results[]` in the
order sent: a failed element does not cancel the rest, a retry happens once, and
`max_bytes` is a budget for the whole call. Each element is its own audit record
and its own unit of rate limit. Writing is always a call of its own.

> The instructions, the tool descriptions and the built-in cheat sheets are
> written in Russian, and so is [`cfg/README.md`](cfg/README.md), the reference
> for the catalogue format. The code and this page are in English.

## Quick start

```sh
make init     # Makefile.mk, cfg/local.toml and cfg/targets.toml from the .dist files
make build    # the ./ringsrv binary
make run      # cfg/local.toml, http://localhost:8075, logging also into run.log
make lint
make test
```

`make init` copies the example catalogue, which points at `example.com` and
carries placeholder credentials — edit `cfg/targets.toml` before expecting an
answer from anything. [`cfg/README.md`](cfg/README.md) is the reference for
every field in it.

Checking it by hand:

```sh
curl -s localhost:8075/status
curl -s localhost:8075/.well-known/oauth-protected-resource | jq .
curl -s -X POST localhost:8075/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | jq -r '.result.tools[0].description'
```

## Connecting a client

Clients reach the server through [`mcpurl`](https://github.com/vmkteam/mcpurl), a
stdio bridge that keeps OAuth and tokens in the OS keychain. TLS is not needed
here: https is the reverse proxy's job in production, not a laptop's.

```sh
make mcp-smoke                 # drive the local server the way a client will
make mcp-install               # register the server in a local MCP client's config
make mcp-install MCP_URL=https://ringsrv.example.com/mcp
```

Claude Code needs no bridge, it speaks HTTP itself:

```sh
claude mcp add --transport http ringsrv http://localhost:8075/mcp
```

A local instance usually runs without authorization while `mcpurl` tries OAuth
before the first request. The `-no-oauth` flag does not yet reach the client
config through `install` (mcpurl 0.0.2), so in `claude_desktop_config.json` it is
added by hand:

```json
{ "command": "mcpurl", "args": ["-no-oauth", "@ringsrv-local"] }
```

There are no server notifications, so `GET /mcp` answers 405, as the
specification allows: a client answered with a 200 and an immediately closed
stream reconnects forever.

## Configuration

Two files, with different owners and different rates of change:

| File | What it describes | Who edits it |
|---|---|---|
| `cfg/local.toml` | the instance: port, OIDC, limits, paths to data | a developer, with a release |
| `cfg/targets.toml` | the catalogue: contour, target profiles, databases, roles, repositories | whoever owns the infrastructure |

The catalogue is validated before the port opens: path patterns are anchored on
`^`, `BaseURL` is https only, any method beyond `GET` requires an explicit
`Write = true`, roles point at targets that exist, and merge / delete / deploy
pass through no pattern — there is a test for that alone.

## Access

An IdP group maps to a role, and a role to a set of tools, targets and
repositories (`[Roles.*]` in the catalogue). The catalogue inside the `api_call`
description is assembled for the caller's role, so a user sees their own targets
and nothing else. Write profiles are always separate, carry their own token, and
are invisible to a role without `AllowWrite`.

## Running the image

```sh
make docker-build
make docker-run
```

The published images are `vmkteam/ringsrv` on Docker Hub and
`ghcr.io/vmkteam/ringsrv`. The image carries the service, git and the
[code-graph-mcp](https://github.com/sdsrss/code-graph-mcp) AST engine; the config
arrives through `RINGSRV_CONFIG`, and the catalogue by mounting `cfg/`.

## Layout

```
cmd/ringsrv/          the entry point
pkg/app/              config, HTTP wiring, routes, DI
pkg/rpc/              the API layer: MCP namespaces, the tool registry, its own models
pkg/ring/             the domain: sessions, jq, the shape of an answer
pkg/ring/target/      targets.toml: parsing, validation, resolving roles
pkg/ring/md/          cheat sheets and scenarios: the content in md/ travels inside the binary
pkg/ring/code/        the code manager: mirrors, the engine, history, blast_radius, why
pkg/ring/proxy/       the api_call pipeline: request → jq → redaction → truncation
pkg/client/upstream/  HTTP to external APIs: profile headers, limits, no redirects
pkg/client/git/       mirrors and worktrees on disk
pkg/client/codegraph/ the code-graph-mcp AST engine
pkg/client/tracker/   parsing YouTrack and Jira answers
pkg/client/gitlab/    parsing a webhook push event
```

The MCP frame — transport, entry, limits, audit, masking, the resource and
prompt catalogue, the answer envelope — lives in
[vmkteam/mcpkit](https://github.com/vmkteam/mcpkit). What stays here is what is
not the frame: the tools, the schemas of their arguments, the target catalogue
and the built-in cheat sheets.

## License

MIT, see [LICENSE](LICENSE).
