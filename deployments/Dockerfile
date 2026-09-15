ARG CI_PROJECT_NAME=ringsrv

# Stage 1: Build backend
FROM golang:1.26-alpine AS builder
ARG CI_PROJECT_NAME
RUN apk add --no-cache git
COPY . /build
WORKDIR /build
RUN CGO_ENABLED=0 go build -mod=vendor -o $CI_PROJECT_NAME ./cmd/$CI_PROJECT_NAME

# Stage 2: Fetch the AST engine (S-58). Downloading happens here so that neither
# curl nor the checksum file end up in the final image. The published binary is
# linked against glibc, which is why the final stage is debian and not alpine.
FROM debian:stable-slim AS engine
ARG CODE_GRAPH_VERSION=v0.127.0
# TARGETARCH is filled in by BuildKit, but only when it is redeclared *without*
# a default: a default wins over the automatic value and silently pins the
# download to one architecture. It used to say amd64 here, so an arm64 build —
# a laptop, an arm CI runner — produced an image whose engine could not start
# at all ("Dynamic loader not found").
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates && rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) asset=code-graph-mcp-linux-x64 ;; \
      arm64) asset=code-graph-mcp-linux-arm64 ;; \
      *) echo "unsupported arch $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    base="https://github.com/sdsrss/code-graph-mcp/releases/download/${CODE_GRAPH_VERSION}"; \
    curl -fsSL -o /tmp/code-graph-mcp "$base/$asset"; \
    curl -fsSL -o /tmp/code-graph-mcp.sha256 "$base/$asset.sha256"; \
    echo "$(cat /tmp/code-graph-mcp.sha256)  /tmp/code-graph-mcp" | sha256sum -c -; \
    chmod +x /tmp/code-graph-mcp

# Stage 3: Final image
FROM debian:stable-slim

ARG CI_PROJECT_NAME
ENV CI_PROJECT_NAME=$CI_PROJECT_NAME

# git is a runtime dependency, not a build one: every code tool shells out to it
# (task 10). It was missing from the previous image, where only the builder had it.
#
# The image is ~385 MB and most of it is not ours: the debian base is 108 MB,
# git another 121 MB — it depends on perl in Debian, which alone is 55 MB. The
# engine adds 41 MB and our binary 24. Alpine would cut the base to 10 MB, but
# the engine ships glibc builds only (no musl asset in the release), which is
# why this stage is debian at all. Excluding docs and manuals was measured and
# saved 2 MB, so it is not here.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata git \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /build/$CI_PROJECT_NAME /opt/$CI_PROJECT_NAME/$CI_PROJECT_NAME
COPY --from=engine /tmp/code-graph-mcp /usr/local/bin/code-graph-mcp

LABEL service_name="$CI_PROJECT_NAME"

WORKDIR /opt/$CI_PROJECT_NAME/

# Exec form rather than shell: in shell form the process runs under /bin/sh and
# SIGTERM reaches the shell instead of the service — no graceful shutdown
# happens, Nomad waits out kill_timeout and finishes the job with SIGKILL. The
# symlink is needed because the exec form does not expand variables, while the
# binary's name comes from CI_PROJECT_NAME.
RUN ln -s "/opt/$CI_PROJECT_NAME/$CI_PROJECT_NAME" /usr/local/bin/entrypoint
ENTRYPOINT ["/usr/local/bin/entrypoint"]
EXPOSE 8085
