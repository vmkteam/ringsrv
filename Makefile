-include Makefile.mk

GOFLAGS=-mod=vendor

PKG := `go list ${GOFLAGS} -f {{.Dir}} ./...`

ifeq ($(RACE),1)
	GOFLAGS+=-race
endif

LINT_VERSION := v2.13.2

MAIN := ${NAME}/cmd/${NAME}

# Where a local client reaches this server. Override for a remote instance:
#   make mcp-install MCP_URL=https://ringsrv.example.com/mcp
MCP_URL ?= http://localhost:8075/mcp

.PHONY: *

# `cp -n` on BSD exits non-zero when the target exists, so `make init` on an
# already-initialised checkout would fail on nothing at all.
#
# The catalogue comes from the laptop copy when the checkout has one, and from
# the example otherwise: a public mirror ships the example alone, and a silent
# `cp` of a missing file would leave `make run` looking for a catalogue that was
# never written.
init:
	@cp -n Makefile.mk.dist Makefile.mk || true
	@cp -n cfg/local.toml.dist cfg/local.toml || true
	@test -f cfg/targets.toml \
		|| cp cfg/targets.local.toml.dist cfg/targets.toml 2>/dev/null \
		|| cp cfg/targets.toml.dist cfg/targets.toml

show-env:
	@echo "NAME=$(NAME)"
	@echo "GOFLAGS=$(GOFLAGS)"

# token: write a fresh Authentik app password into cfg/targets.toml. The dev
# outpost accepts nothing but HTTP Basic "login:app-password", and an app
# password lives no longer than 30 minutes, so the base64 would otherwise be
# computed by hand every session. Only the value after "Basic" in [Defaults] is
# replaced; an "expires at HH:MM" comment above the section, if there is one,
# gets the time 30 minutes from now.
#   make token TOKEN=<app-password>            # login is $USER
#   make token TOKEN=<app-password> TOKEN_USER=ringsrv
TOKEN_USER ?= $(USER)
TARGETS_FILE ?= cfg/targets.toml

token:
	@test -n "$(TOKEN)" || (echo "usage: make token TOKEN=<authentik app password> [TOKEN_USER=$(TOKEN_USER)]" && exit 1)
	@test -f $(TARGETS_FILE) || (echo "$(TARGETS_FILE) not found — run make init" && exit 1)
	@sed -n '/^\[Defaults\]/,/^\[/p' $(TARGETS_FILE) | grep -q 'Authorization: Basic ' \
		|| (echo "no 'Authorization: Basic' header in [Defaults] of $(TARGETS_FILE)" && exit 1)
	@basic=$$(printf '%s:%s' '$(TOKEN_USER)' '$(TOKEN)' | base64 | tr -d '\n'); \
	expires=$$(date -v+30M +%H:%M 2>/dev/null || date -d '+30 min' +%H:%M); \
	sed -i.bak \
		-e "/^\[Defaults\]/,/^\[/ s|Authorization: Basic [^\"]*\"|Authorization: Basic $$basic\"|" \
		-e "s|expires at [0-9:]*|expires at $$expires|" \
		$(TARGETS_FILE) && rm -f $(TARGETS_FILE).bak; \
	echo "$(TARGETS_FILE): Authorization: Basic for $(TOKEN_USER) written, expires by $$expires"

tools:
	@curl -sfL https://raw.githubusercontent.com/golangci/golangci-lint/${LINT_VERSION}/install.sh | sh -s -- -b $(go env GOPATH)/bin ${LINT_VERSION}

fmt:
	@golangci-lint fmt

lint:
	@golangci-lint version
	@golangci-lint config verify
	@golangci-lint run

build:
	@CGO_ENABLED=0 go build $(GOFLAGS) -o ${NAME} $(MAIN)

# run: the local server. The log is duplicated into run.log (RUN_LOG=… to
# change it) so that it can be read from another window, or handed to an
# assistant while the server keeps running, instead of being copied out of the
# terminal. The terminal gets it as is, the file without ANSI codes: the dev
# logger colours its output without asking whether a terminal is there at all.
RUN_LOG ?= run.log

run:
	@echo "Compiling"
	@go run $(GOFLAGS) $(MAIN) -config=cfg/local.toml -dev 2>&1 \
		| tee /dev/tty \
		| perl -pe 'BEGIN { $$| = 1 } s/\e\[[0-9;]*m//g' > $(RUN_LOG)

# config-checks validates both halves of the configuration, and the two are of
# a different kind.
#
# Git holds the deployment templates and the reference catalogue: those are
# held by tests — a section that exists in no contour, a catalogue that drifted
# away from the heredoc, a key in a place where OIDC.Required will not accept
# it anyway. A contour holds the rendered cfg/*.toml, which no test has ever
# seen: the service itself reads them, and until now their only reader was
# startup — a poor moment to learn about a missing comma.
#
# CONFIG picks the instance to check: make config-checks CONFIG=cfg/prod.toml.
#
# The target is deliberately absent from CI: it is run next to a config change,
# not on every build. Merely leaving it out of CI was not enough — the template
# tests live in ./pkg/app and ./pkg/ring/target, which CI runs in full, so an
# edit to the prod config painted the build of any branch red. Now those tests
# wait for RINGSRV_CHECK_CONFIGS (targettest.SkipUnlessConfigChecks) and mark
# themselves skipped without it; only this target sets the variable.
CONFIG ?= cfg/local.toml

config-checks:
	@echo "Shipped templates and the example catalogue"
	@RINGSRV_CHECK_CONFIGS=1 go test -count=1 $(GOFLAGS) ./pkg/app ./pkg/ring/target
	@echo "$(CONFIG) and the catalogue it names"
	@go run $(GOFLAGS) $(MAIN) -check-config -verbose -config=$(CONFIG)

# mcp-smoke: drive the local server through mcpurl exactly as a client will.
# This is the Q-12 check: one handshake, one tools/list, and the bridge's own
# log of what it saw. No TLS and no caddy — clients reach us through mcpurl
# over stdio, so https is the reverse proxy's problem in production, not the
# laptop's.
mcp-smoke:
	@command -v mcpurl >/dev/null || (echo "mcpurl not found — brew install vmkteam/tap/mcpurl" && exit 1)
	@printf '%s\n%s\n%s\n' \
		'{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"mcp-smoke","version":"1"}}}' \
		'{"jsonrpc":"2.0","method":"notifications/initialized"}' \
		'{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
		| mcpurl -no-oauth -v $(MCP_URL) 2>&1 | grep -v -i authorization

# mcp-install: register this server in a local MCP client through mcpurl.
# `mcpurl install` writes the client config and keeps the OAuth tokens in the
# OS keychain, so no token ever lands in a config file we edit.
mcp-install:
	@command -v mcpurl >/dev/null || (echo "mcpurl not found — brew install vmkteam/tap/mcpurl" && exit 1)
	@mcpurl install $(MCP_URL)

generate:
	@go generate ./pkg/rpc

# Live database tests re-create their fixtures on the shared stands, so with
# RINGSRV_TEST_PG / RINGSRV_TEST_CH set the packages run one at a time.
TEST_PARALLEL := $(if $(RINGSRV_TEST_PG)$(RINGSRV_TEST_CH),-p 1,)

test:
	@echo "Running tests"
	@go test -count=1 $(GOFLAGS) $(TEST_PARALLEL) -coverprofile=coverage.txt -covermode count $(PKG)

test-short:
	@go test $(GOFLAGS) -v -test.short -coverprofile=coverage.txt -covermode count $(PKG)

test-race:
	@CGO_ENABLED=1 go test $(GOFLAGS) $(TEST_PARALLEL) -race -v -test.short $(PKG)

mod:
	@go mod tidy
	@go mod vendor
	@git add vendor

# docker-build / docker-run: build and exercise exactly the image that goes to
# Nomad. It checks what `go test` cannot see: that the binary links statically,
# that git and the AST engine ended up in the runtime layer, and that the
# service starts with a config taken from an environment variable rather than
# an argument — the image's ENTRYPOINT accepts no arguments.
IMAGE ?= ringsrv:local
# Source directories are mounted read-only: a local targets.toml clones
# repositories over file://, and without them code_* inside the container would
# answer "no such repository" — not because anything is wrong with the image.
CODE_MOUNT ?= $(HOME)/Projects/Go

docker-build:
	@docker build -f deployments/Dockerfile -t $(IMAGE) .

docker-run:
	@test -f cfg/docker.toml || cp cfg/docker.toml.dist cfg/docker.toml
	@docker run --rm -it -p 8085:8085 \
		-e RINGSRV_CONFIG=/opt/ringsrv/cfg/docker.toml \
		-e RINGSRV_VERBOSE=true \
		-v $(PWD)/cfg:/opt/ringsrv/cfg:ro \
		-v ringsrv-data:/data \
		-v $(CODE_MOUNT):$(CODE_MOUNT):ro \
		--name ringsrv-local $(IMAGE)

# nomad-check: parse the job specification with both variable files the same
# way CI does. `job run -output` prints JSON and never reaches the server, so
# no cluster is needed for it and nothing is deployed.
#
# Errors at this layer are otherwise visible only from the pipeline and are not
# caught by eye: `${file(...)}` in *.vars.hcl looked flawless, but Nomad reads
# variable files as plain values and answers "Function calls not allowed"; the
# catalogue body now lives in them as a string, and the same check catches a
# heredoc without a newline after EOF — Nomad reports that one as "Unterminated
# template string" on the file's last line, far from the edit itself.
NOMAD_IMAGE ?= hashicorp/nomad:1.10.5

nomad-check:
	@for vars in devel master; do \
		printf '%-8s ' "$$vars"; \
		docker run --rm -v $(PWD):/w -w /w \
			-e NOMAD_VAR_image_version=check -e NOMAD_VAR_commit_sha=0000000 \
			$(NOMAD_IMAGE) job run -output \
			-var-file deployments/$$vars.vars.hcl deployments/service.nomad.hcl \
			> /dev/null || exit 1; \
		echo "ok"; \
	done
