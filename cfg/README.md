# Configuration: the instance and the target catalogue

There are two files, and they have different owners and different rates of change.

| File | What it describes | Who edits it, and when |
|---|---|---|
| `local.toml`, or whatever a deployment renders | the service itself: port, OIDC, Sentry, limits, paths on disk | together with a release |
| `targets.toml` | the **catalogue**: which APIs and databases may be reached, who is allowed to, which repositories the service knows | between releases, by whoever is on call |

The catalogue needs no image rebuild. In production the real file is rendered by
the deployment into the allocation's tmpfs with the tokens pulled from a secret
store, so no secret is in git and none lands on a node's disk.

The references in this directory:

- `targets.toml.dist` — an example catalogue on `example.com` hosts with literal
  placeholder credentials. `make init` copies it to `targets.toml`;
- `local.toml.dist` — the instance config for `make run`;
- `docker.toml.dist` — the instance config for `make docker-run`.

**The catalogue is validated as a whole at startup, and the service does not
start when anything in it is wrong**: a half-loaded catalogue reads to a user as
a permissions problem and gets debugged from the wrong end of the wire.

The examples below are anonymised: `*.example.com` hosts, `<org>` for an
organisation, `{{ .x }}` for the variable names of your own deployment.

---

## Filling it in: the order

1. **The contour.** `Env = "dev"` or `"prod"`. One catalogue is one contour, and
   every target's description starts with it.
2. **The pass.** If the hosts of a contour sit behind a shared forward-auth
   (Authentik, oauth2-proxy, a gateway header), the pass header goes into
   `[Defaults]`. Hosts with an authorization of their own get
   `SkipDefaultHeaders = true`.
3. **Profiles.** One `[Profiles.<name>]` per system: `BaseURL`, its own header,
   `AllowMethods`, `AllowPaths` taken from the cheat sheet in
   `pkg/ring/md/md/targets/<name>.md`, `DefaultJQ`. Write profiles are separate
   `<name>-rw` sections.
4. **Databases.** One `[Databases.<name>]` per database with a read-only user —
   see [Read-only database users](#read-only-database-users).
5. **Repositories.** `[Repos.<name>]` with `CloneURL`, `CloneTarget`, a branch,
   layers, exclusions, issue keys, and the bindings to Sentry, Nomad and
   Prometheus.
6. **Roles.** `[Roles.<name>]` — an IdP group mapped to tools, targets,
   databases (explicitly) and repositories.
7. **Secrets.** Every `{{ .x }}` is a key in the deployment's secret store; on a
   laptop it is a literal in `cfg/targets.toml`, which is in `.gitignore`.
8. **Checking.** `go test ./pkg/ring/target/` runs the validator; a live
   instance is `make build`, a `local.toml`, one call per target and a
   `db_introspect` per database.

---

## `Env` — the contour

```toml
Env = "dev"   # or "prod"
```

The contour is named in the same file as the hosts so that the two cannot drift
apart. The `Description` of every profile and database must start with the
contour — which is why a prod catalogue dropped into a dev instance fails
validation.

## `WebhookSecret` — the GitLab webhook

```toml
WebhookSecret = "{{ .gitlabWebhookSecret }}"
```

The secret for `POST /v1/webhook/gitlab`: on a push event a mirror is updated at
once instead of waiting for the fifteen-minute cron. An empty value disables the
endpoint. It lives here because this file already carries tokens.

## `[Defaults]` — what every profile inherits

```toml
[Defaults]
Headers = ["Authorization: Basic {{ printf `%s:%s` .proxyUser .proxyPassword | base64Encode }}"]
```

`Headers` are added to every request to every profile: this is the pass through
the contour's shared forward-auth, and without it a closed host answers with a
redirect to a login page. It is declared once rather than in every profile —
otherwise the eighth target gets added without it.

- a profile header of the same name **overrides** the shared one (names are
  compared case-insensitively). If a host sits behind forward-auth it cannot
  have an `Authorization` of its own: that would displace the pass. Such a
  target needs a header under a different name (`X-Nomad-Token`, `PRIVATE-TOKEN`);
- `SkipDefaultHeaders = true` on a profile turns the shared headers off
  entirely — for a host with an authorization of its own;
- the format is the same as for profile headers: `"Name: value"`. `base64Encode`
  above is a template function of the deployment; on a laptop the value is
  written by `make token TOKEN=<password>` (the login is `$USER`, overridden by
  `TOKEN_USER`).

## `[Profiles.<name>]` — one external HTTP API

```toml
[Profiles.prom]
Description  = "dev · Prometheus: service metrics, job = service name"
BaseURL      = "https://prometheus.example.com"
AllowMethods = ["GET"]
AllowPaths   = [
  "^/api/v1/query$",
  "^/api/v1/query_range$",
  "^/api/v1/series$",
]
DefaultJQ    = ".data.result"
MaxBytes     = 32768
TimeParams   = ["start", "end", "time"]
TimeFormat   = "unix"

[Profiles.grafana]
Description        = "dev · Grafana: dashboards, annotations, logs through a datasource"
BaseURL            = "https://grafana.example.com"
SkipDefaultHeaders = true                         # its own authorization, no forward-auth in front
Headers            = ["Authorization: Bearer {{ .grafanaToken }}"]
AllowMethods       = ["GET", "POST"]
ReadOnlyPost       = true                         # reads through POST, but does not write
AllowPaths         = [
  "^/api/search$",
  "^/api/annotations$",
  "POST ^/api/ds/query$",                         # the single POST is named explicitly
]

[Profiles.sentry]
Description        = "dev · Sentry: issues, events, releases; environment pinned to the contour"
BaseURL            = "https://sentry.example.com"
SkipDefaultHeaders = true
Headers            = ["Authorization: Bearer {{ .sentryToken }}"]
AllowMethods       = ["GET"]
AllowPaths         = ["^/api/0/organizations/<org>/issues/$", "^/api/0/issues/[^/]+/events/latest/$"]
Query              = ["environment=devel"]        # one instance serving two contours
Redact             = "warn"
RedactRules        = ["email", "phone", "card", "token", "jwt", "pem", "ip"]

[Profiles.tracker-rw]
Description        = "dev · tracker: create an issue, create a comment"
BaseURL            = "https://tracker.example.com"
SkipDefaultHeaders = true
Headers            = ["Authorization: Bearer {{ .trackerRwToken }}"]
AllowMethods       = ["GET", "POST"]
AllowPaths         = ["^/api/issues$", "^/api/issues/[A-Z]+-\\d+/comments$"]
Write              = true                         # only roles with AllowWrite see it
```

| Field | Required | Meaning |
|---|---|---|
| `Description` | yes | **the model reads this.** Starts with the contour (`dev · …`), then what can be asked here and one or two rules of the target. All descriptions together with the databases' must fit in 4 KB |
| `BaseURL` | yes | `scheme://host[:port]`, https only; http is allowed for loopback and `*.service.consul` names alone — inside a datacentre Prometheus and Nomad answer on nothing else. No path, no query, no credentials |
| `Headers` | — | authorization headers as `"Name: value"`; the value must be non-empty — an empty one produces a 401 that looks like a network fault |
| `AllowMethods` | yes | upper case. Any method other than `GET` requires `Write = true` or `ReadOnlyPost = true` |
| `AllowPaths` | yes | regular expressions, **each anchored with `^` and ended explicitly: `$` or `/`**; an optional method prefix: `"POST ^/api/ds/query$"` |
| `DefaultJQ` | — | the default filter; a call may override it, and `jq: "."` returns the raw body. Compiled at load; an answer filtered by it is marked `default_jq: true` |
| `MaxBytes` | — | the answer ceiling for this target; 0 → the instance's `Limits.MaxBytes`. A call's argument may not exceed 262144 |
| `TimeParams` / `TimeFormat` | — | the query parameters where `now-1h` / `now-24h` / `now-7d` are expanded by the server, and into what: `unix`, `unix_ms`, `unix_ns`, `rfc3339` |
| `TimeBodyParams` | — | the same for a JSON body, by dotted paths (`params.start`); the unix formats become a number and `rfc3339` a string; a batch is handled element by element. Requires `TimeFormat` and a method with a body. An entry may name its own format after a colon — `"params.filter.from:rfc3339"` — for an upstream whose windows differ in format; then `TimeFormat` is only needed by the entries without a suffix |
| `AllowRPCMethods` | — | JSON-RPC behind a single path: the methods a body may carry (an object or a batch, case-insensitive); everything else is `RPCMethodNotAllowed`. On a `ReadOnlyPost` profile a known writing method is not accepted |
| `AllowQueryParams` | — | the query parameters a call may set; a name may carry a bound on its value: `"query"`, `"step>=15s"`, `"limit<=100"`. A bound is an integer or a duration (`15s`, `1h30m`); the value is read as a number or as a duration in seconds, and a bounded value may not be negative. Empty means any; anything outside the list or the bound, and a `#` in the path, is `QueryParamNotAllowed`. Credential names — `token` and `*_token`, `api_key`/`apikey`/`api-key`/`x-api-key`, `secret`/`client_secret`/`password`, `auth`/`authorization`/`bearer`, `sudo` — pass on no profile, neither in the query nor as a top-level key of a JSON body |
| `Query` | — | `"key=value"` pairs the server puts on every request, replacing the model's value for the same key: the contour of a shared instance is a property of the target, not an argument of a call |
| `Redact` / `RedactRules` | — | PII redaction in the answer: `off` (default), `warn` (count, do not cut), `redact`. The rules are `email`, `phone`, `card`, `token`, `jwt`, `pem`, `ip`; an empty list means all of them |
| `SkipDefaultHeaders` | — | do not add the headers from `[Defaults]` |
| `AllowHeaders` | — | the header names a **call** may set itself; empty means none. A name on this list must not be sent by the profile itself, and the forbidden families are never accepted |
| `Write` | — | the profile changes state; roles without `AllowWrite` do not see it. Mutually exclusive with `ReadOnlyPost` |
| `ReadOnlyPost` | — | the API reads through POST; on such a profile an unprefixed pattern is GET only, and the POST paths are named explicitly. Nothing beyond GET and POST |

**How to write `AllowPaths`.** The path is matched without the query, so
`^/api/v1/query$` also permits `/api/v1/query?query=up`. End a pattern with `$`
for an exact path or `/` for a subtree; without an explicit end the validator
rejects it: `^/api/annotations` would also match `/api/annotations/graphite`.
For placeholders use `[^/]+`, `\d+`, `[A-Z]+-\d+` — the narrower the better.

**Headers from a call.** By default only the profile's own headers reach an
upstream: a header the model can write is a header an injection from somebody
else's answer can write too. `AllowHeaders` opens named exceptions for APIs
where the credentials belong to the caller rather than to the service:

```toml
AllowHeaders = ["Authorization2", "Platform"]
```

The rules, checked at load time:

* a profile header always wins — a name the profile sends itself is not accepted
  in `AllowHeaders`, because the permission would be a silent no-op;
* `Host`, `Cookie`, `Content-Length`, the hop-by-hop headers, `X-Impersonate`
  and the whole `X-Forwarded-*` and `X-Authentik-*` families are never accepted:
  those are somebody else's idea of who is calling;
* `Authorization` is deliberately not on the forbidden list — an API with
  per-caller credentials has to be expressible, and what protects here is not a
  banned word but the pair "the name is stated explicitly" plus "the profile
  sends none of its own".

Names are canonicalised, so `platform` and `Platform` are one header and two
spellings in one call are rejected. Values are at most 1024 bytes, free of
control characters, and at most eight headers per call. The audit records **the
names, not the values**: the reason to set a header from a call is usually a
secret. The `headers` argument appears in the `api_call` schema only for a role
where at least one target accepts it — it is paid for on every `tools/list`. The
accepted names are appended to the target's line, because otherwise the only way
to learn them is a refusal.

**A method in a pattern.** `"POST ^/api/ds/query$"` binds a pattern to one
method. On a `ReadOnlyPost` profile this is mandatory. On a write profile a
pattern without a prefix applies to every entry of `AllowMethods`, and a prefix
narrows it.

**Encoding.** The path is matched as written (some APIs read a file path as a
single segment only with `%2F`), but the decoded form is checked separately:
`%2e%2e`, `%00` and `%5c` are rejected before matching. Upstream redirects are
not followed: a 3xx is returned to the model with its `Location`.

**What will never pass.** The validator runs samples of merge, delete, deploy,
annotation creation and the datasource proxy against every profile: a pattern
that lets one of them through fails the load, however well-meant the widening
was.

## `[Databases.<name>]` — a read-only database

```toml
[Databases.orders-pg]
Description   = "dev · PostgreSQL, a replica of orders-api: orders, payments; the enums (statusId) live in code, see code in db_introspect"
Driver        = "postgres"                        # or clickhouse
Addr          = "db-replica.example.com:5432"     # host:port of the native protocol, no scheme and no TLS
Database      = "orders"
User          = "ringsrv_ro"
Password      = "{{ .ordersPgRoPassword }}"
Schemas       = ["public", "billing"]             # postgres only: the search_path, and what db_introspect shows
Repo          = "orders-api"                      # the repository with a db layer — the default code pointer
Redact        = "redact"                          # mandatory for postgres; off is written out too
RedactRules   = ["email", "phone"]
MaxRows       = 200                               # 0 → the instance's Limits.DBMaxRows
Timeout       = "15s"                             # a string; 0 → Limits.DBTimeout
MaxConcurrent = 2                                 # the pool and the concurrent queries; 0 → 2

# The schemas of one database belong to different services, and public to two of
# them at once: the code pointer of db_introspect is handed out per schema. A
# schema without a line of its own reads Repo; the owner is written first.
[Databases.orders-pg.SchemaRepos]
public  = ["orders-api", "billing-api"]
billing = ["billing-api"]

[Databases.events-ch]
Description = "dev · ClickHouse: events, anonymised; now() - INTERVAL 2 HOUR, and FINAL on a ReplacingMergeTree"
Driver      = "clickhouse"
Addr        = "clickhouse.example.com:9000"
Database    = "events"
User        = "ringsrv"
Password    = "{{ .clickhouseRoPassword }}"
Redact      = "off"
Repo        = ["events-api", "reports-api"]       # two services write it; ClickHouse has no schemas to split by
```

A target for `db_query` and `db_introspect`. It is a section of its own rather
than a profile: none of the HTTP checks apply to a database, and databases are
reachable from the internal network only. The name shares a namespace with the
profiles: a role lists both in `Targets`.

| Field | Required | Meaning |
|---|---|---|
| `Description` | yes | starts with the contour; two rules for the model — the dialect in one example, and where the enums live in code. The schema itself the model learns from `db_introspect`, so a database needs no cheat sheet |
| `Driver` | yes | `postgres` or `clickhouse` |
| `Addr` | yes | `host:port`; the port is 1..65535, no scheme, no path, no credentials |
| `Database` | yes | the database name; for ClickHouse also the default for unqualified names |
| `User` / `Password` | yes | the read-only user of this target; one per target, and the audit knows who asked |
| `Schemas` | — | `postgres` only: identifiers without quotes; empty → `public` |
| `Repo` | — | a name from `[Repos.*]`, or a list of them — a database may be written by more than one service. `db_introspect` answers with `code: {repos: [{repo, layer}], hint}`. The pointer leads to the `db` layer, or to `domain` when there is none |
| `SchemaRepos` | — | `postgres` only: schema → list of repositories, overriding `Repo` for that schema. A key must be in `Schemas`, and the list non-empty and free of repeats. The `hint` is one `code_search` across all of them when their layer matches, and one call per repository otherwise |
| `Redact` / `RedactRules` | postgres: yes | the redaction mode for answer rows; SQL in the audit is redacted always, whatever the mode |
| `MaxRows` / `Timeout` / `MaxBytes` / `MaxConcurrent` | — | zeros take the defaults of the instance and the client; `Timeout` is a string (`"15s"`) and a bare number is rejected |

**There is no writing in any form** — no flag and no separate profile. The user
is read-only on the database side, and the server proves it with probes at
startup: a reachable database that accepts a write refuses the start, an
unreachable one leaves the target `unproven` — queries do not go to it, and a
probe is retried every minute.

### Read-only database users

**PostgreSQL.** The role must not be a superuser, must not create roles or
databases, must not bypass RLS, must own no tables, and must hold no privilege
beyond `SELECT` — neither on tables nor on individual columns. Grants may go to
a group role the user belongs to (`GRANT readonly TO ringsrv_ro`) rather than to
the user itself; the probe does not ask which groups a role is in, it walks
every role reachable through `SET ROLE` and refuses if any of them can write.
One condition comes with that: a role working through a group must be `INHERIT`,
or the probe passes while queries answer "permission denied", because without
`SET ROLE` the group's grants do not apply. Grant per table rather than per
schema: tables without PII in full, tables with PII by column, leaving out
payment details, API keys, passwords and TOTP secrets. `USAGE` is needed on
every schema listed in `Schemas`, or `db_introspect` will not show its tables.
The server also needs a `pg_hba.conf` line for the user, from the addresses it
calls from, with `scram-sha-256`.

**ClickHouse.** The user needs `readonly ≠ 0`, `allow_ddl = 0`, and nothing in
`SHOW GRANTS … FINAL` beyond `SELECT`, `SHOW` and `dictGet`. Pin the settings
with `READONLY` so that they cannot be raised by a query; the server learns this
from a probe and then does not send the pinned settings with a query. Do not
grant `SOURCES`: the `url()`, `s3()`, `remote()`, `postgresql()` and `mysql()`
table functions read other hosts with credentials written in SQL, which is SSRF
from inside the database.

## `[Roles.<name>]` — who can do what

```toml
[Roles.viewer]
Description = "Reading metrics and logs"
Groups      = ["mcp-users"]
Tools       = ["api_call", "repo_map"]
Targets     = ["grafana", "prom", "sentry"]

[Roles.developer]
Description = "Everything read-only, plus code, history and data"
Groups      = ["mcp-developers"]
Tools       = ["api_call", "repo_map", "code_read", "code_search", "code_history", "code_refs", "blast_radius", "why", "db_query", "db_introspect"]
Targets     = ["grafana", "prom", "sentry", "gitlab", "tracker", "orders-pg", "events-ch"]
Repos       = ["*"]

[Roles.oncall]
Description = "On call: everything read-only plus writing to the tracker"
Groups      = ["mcp-oncall"]
Tools       = ["*"]
Targets     = ["*", "orders-pg", "events-ch"]     # "*" covers profiles only; databases are named
Repos       = ["*"]
AllowWrite  = true
MaxWrites   = 10                                  # write calls per session
```

A role ties IdP groups to a set of tools, targets, databases and repositories,
so that "what group X sees" is read in one place and shows up in a diff.

- `Groups` may not be empty: a role nobody can reach reads as access granted;
- `Tools` accepts only known names — `api_call`, `repo_map`, `code_read`,
  `code_search`, `code_history`, `code_refs`, `blast_radius`, `why`, `db_query`,
  `db_introspect`; `"*"` is all of them;
- `Targets` lists profiles and databases together, but **`"*"` expands to
  profiles only** — a database is named explicitly, even next to a star;
- `Repos = ["*"]` is every repository in the catalogue;
- a write profile in `Targets` requires `AllowWrite = true`, or the catalogue
  will not load; a role without `AllowWrite` does not see write profiles;
- `MaxWrites` caps writes per session; `0` means no cap, and it only makes sense
  together with `AllowWrite`;
- the code tools are visible to a role only with repositories, and the data
  tools only with databases: the listing and the call say the same thing.

## `[Repos.<name>]` — a repository

```toml
[Repos.orders-api]
GitLabProject = 42
Description   = """
Orders API. Layers: db → domain → rpc.
Gotchas: the payment queue does not survive a restart without draining; the
database is shared with billing.
"""
CloneURL      = "https://git.example.com/<org>/orders-api.git"
CloneTarget   = "gitlab"                          # the clone token comes from the gitlab profile
DefaultBranch = "master"
Refspec       = ["+refs/heads/*:refs/remotes/origin/*"]
Tags          = false
SentrySlug    = "orders-api"
NomadJob      = "orders-api"
PromJob       = "orders-api"
IssueTarget   = "tracker"                         # the tracker profile `why` enriches from
IssuePath     = "/api/issues/{id}?fields=summary,description,comments(text)"
Exclude       = ["vendor/", "node_modules/", "*.pem", ".env*", "id_rsa*"]
TaskIDRegexp  = "^([A-Z]+-\\d+)"

[Repos.orders-api.Layers]
db     = "pkg/db"
domain = "pkg/orders"
rpc    = "pkg/rpc"
```

| Field | Required | Meaning |
|---|---|---|
| `CloneURL` | yes | the https clone address; `file://` is for a local stand and the tests only |
| `CloneTarget` / `CloneToken` | one of | where the clone token comes from: the name of a read profile on the same host carrying a single token header (`PRIVATE-TOKEN` or `Bearer`), or the token itself as a placeholder. Not both |
| `DefaultBranch` | yes | the default branch; the code tools never take `HEAD` silently |
| `Refspec` / `Tags` | — | what to pull into the mirror; tags are usually not needed |
| `GitLabProject` | — | the project id the webhook finds the repository by |
| `SentrySlug` / `NomadJob` / `PromJob` | — | what the service is called in the other systems — this is what `repo_map` hands out; a utility without a job of its own leaves them empty |
| `IssueTarget` / `IssuePath` | — | **the name of a tracker profile** (the enrichment in `why` runs under the caller's role) and the request for one issue with `{id}`; the path must pass that profile's allowlist. An empty `IssuePath` means YouTrack |
| `Layers` | — | layer → directory; a database with `Repo` needs `db` or `domain`, which is where the code pointer leads. Paths are relative and may not contain `..` |
| `Exclude` | — | what is never handed out: directory prefixes (`vendor/`) and file globs (`*.pem`) |
| `TaskIDRegexp` | — | what an issue key looks like in a commit subject; the capture group is the key itself |
| `Description` | — | a hand-written description of the architecture: layers, gotchas, dependencies |

The section name becomes the mirror's directory: letters, digits, `._-`, no `/`.

## Secrets

- The `.dist` files carry placeholders only. In a deployment the values live in
  its secret store and the template substitutes them at render time; a
  missing key should fail the deploy rather than turn into `<no value>` and a
  "password authentication failed" in the middle of the night.
- On a laptop it is `cfg/targets.toml`, which is in `.gitignore`, with literals;
  `make init` creates it from `targets.toml.dist`.
- Secrets are not logged, not returned and never reach the text of an error: a
  database password is cut out of any error the DBMS produces before it is sent.

## What is checked at load

The validator refuses as a whole, naming the section and the field. The main
points:

- `Env` is `dev` or `prod`, and the `Description` of a profile and a database
  starts with the contour;
- headers in `[Defaults]` and in profiles are `"Name: value"` with a non-empty
  value;
- a profile has a `Description` and non-empty `AllowMethods` and `AllowPaths`;
  every pattern compiles, is anchored with `^` and ends with `$` or `/`; a
  method prefix comes from `AllowMethods`;
- `BaseURL` is https and has no path; a method beyond `GET` requires `Write` or
  `ReadOnlyPost`, and the two flags are mutually exclusive; `ReadOnlyPost`
  allows GET and POST only and requires a `POST`-prefixed pattern;
- the samples of merge, delete, deploy, annotations and the datasource proxy
  pass through no profile;
- `DefaultJQ` compiles; `TimeFormat` is one of four; `Query` is `key=value`
  without `&?#` or spaces; `Redact` is one of three and its rules are known;
- `TimeBodyParams` and `AllowRPCMethods` only appear on a profile with a method
  beyond `GET`; paths have no empty segments and no repeats (including one path
  in two formats), a format after a colon is one of four, methods do not repeat,
  and on `ReadOnlyPost` none of them writes;
- `AllowQueryParams` names carry no `&?#=` or spaces, a `<=`/`>=` bound is an
  integer or a duration, and nothing repeats; a credential name, a key already
  in `Query` (the profile would replace it anyway) and a bound on a parameter
  from `TimeParams` are not accepted; every `TimeParams` entry must be on the
  list;
- `[Databases.*]`: the name does not collide with a profile, `Driver` is one of
  two, `Addr` is `host:port`, `Database`/`User`/`Password` are non-empty, the
  limits are ≥ 0, `Timeout` is at least a second, `Redact` is named for
  `postgres`, `Schemas` are identifiers and `postgres` only, every repository in
  `Repo` and `SchemaRepos` has a `db` or `domain` layer and does not repeat, and
  the `SchemaRepos` keys come from `Schemas`;
- a role points at profiles, databases and repositories that exist, its `Tools`
  are known and its `Groups` non-empty; a write profile requires `AllowWrite`;
- `IssueTarget` is an existing profile and `IssuePath` with a key substituted
  passes its allowlist; `CloneTarget` is a read profile on the `CloneURL` host
  with a single token header;
- the profile and database descriptions together are ≤ 4 KB, and the
  `tools/list` of the widest role is ≤ 12.5 KiB, which a test checks.

To check a catalogue without starting the service:

```sh
go test ./pkg/ring/target/            # the validator
make test                             # plus the tools/list budget and the cheat sheets
./ringsrv -check-config -verbose -config=cfg/local.toml
```

## Local stands for the databases

The tests in `pkg/db` and `pkg/chdb`, and the live test in `pkg/rpc`, run
against real databases and skip themselves without the variables:

```sh
# PostgreSQL from brew with any database; ClickHouse as a container on loopback:
docker run -d --name ringsrv-ch -p 127.0.0.1:9000:9000 -p 127.0.0.1:8123:8123 \
  -e CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1 -e CLICKHOUSE_DB=app clickhouse/clickhouse-server:24.10

export RINGSRV_TEST_PG='postgres://$USER@127.0.0.1:5432/app?sslmode=disable'   # someone who can create roles and schemas
export RINGSRV_TEST_CH='clickhouse://default:@127.0.0.1:9000/app'              # someone who can create users
make test                                                                      # with the variables set, the packages run one at a time
```

The fixtures create the `ringsrv_test` schema and the `ringsrv_test_*` users on
every run themselves.

## Common mistakes

| Symptom | Cause |
|---|---|
| a target answers 302 to a login page | the host is behind forward-auth and the profile overrode the shared `Authorization` with its own — it needs a header under a different name |
| 200 and HTML | the client made it as far as the login page; check `[Defaults]` and whether the pass password has expired |
| `PathNotAllowed` on a path that exists | a pattern without `$` or `/`, or without a `POST` prefix on a `ReadOnlyPost` profile |
| `RPCMethodNotAllowed` on a method that certainly exists | the method is not in `AllowRPCMethods`, or the body is neither a single JSON-RPC request nor a batch |
| `QueryParamNotAllowed` on a parameter the API accepts | the name is not in the profile's `AllowQueryParams`, the value is outside its bound, there is a `#` in the path, or it is a credential name in the query or among the top-level keys of a JSON body — those pass nowhere |
| the start fails with `unknown keys: Profiles.*.<field>` | the image is older than the catalogue: the catalogue carries a field this binary does not know. The catalogue travels with the deployment and the image separately; when rolling an image back, roll the catalogue back too, and roll a new field out after the image |
| `why` answers `no-access` | the role does not have the profile named in `IssueTarget` |
| a database is `unproven: … authentication failed` | the wrong secret or password; check that the deployment actually substituted it |
| the start fails with "reachable but not read-only" | the database user was given more than SELECT, or the role is a member of other roles, or `dblink` is installed in the database |
| `db_query` → `Timeout` on a trivial query | `Timeout` was written as a number (nanoseconds); it must be the string `"15s"` |

## How to add something new

**A new target.** A profile with its token; the name goes into the `Targets` of
the roles that should see it. Then the cheat sheet
`pkg/ring/md/md/targets/<name>.md`: without it the target cannot be used from
Claude Desktop, and a test requires it. The paths in the cheat sheet are run
through the same profile's allowlist. Write `AllowQueryParams` for an API with a
surveyable set of parameters (Prometheus, Nomad, YouTrack, Slack), with bounds
on `limit` and `step`; for an API with hundreds of them (GitLab, Sentry,
Grafana) leave the list out. The cheat sheet lists the same names in its "query
parameters" paragraph, and a test compares the two in both directions.

**A new database.** A `[Databases.*]` section with a read-only user; the
password goes into the deployment's secrets, and the name goes explicitly into
the `Targets` of the roles — a star does not grant a database. A database needs
no cheat sheet: there is already one per driver (`tools/db.md`), and the model
reads the schema from `db_introspect`. List `Schemas` and `SchemaRepos`
immediately: a forgotten schema is invisible in `db_introspect`, and a schema
without an owner sends the model looking for models across every repository.
Every database costs room in the tool description — the budget is checked by
`TestToolSurfaceFitsBudget`.

Whether to use a separate database or a schema inside an existing one is not a
formatting question: databases have their own migrations and their own set of
services, so databases are kept apart as targets (two databases on one server
are two targets) and schemas inside a database are handled through
`SchemaRepos`. ClickHouse has no schemas: a database written by several services
is described by a list in `Repo`, and the model picks from it by table name.

**A new repository.** A `[Repos.*]` section with `CloneURL`, `CloneTarget`,
`DefaultBranch`, the bindings and `TaskIDRegexp`; add the layers and `Exclude`
right away, or the first `code_search` comes back full of `vendor/`. The webhook
in the GitLab project points at `/v1/webhook/gitlab` with the secret from the
catalogue.

**A new role.** The group in the IdP, then a `[Roles.*]` section. Check that the
group really arrives in the token, in the claim named by `OIDC.GroupsClaim`: a
role that matched no group answers `403` rather than with an empty catalogue.
