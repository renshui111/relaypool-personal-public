# Build the relaypool gateway.
#
# One Go stage produces a static binary with the web console embedded (the
# console build has no compiler step — web/dist is committed and only needs a
# macro substitution, done by scripts/build-web.mjs when developing).
#
# CGO is disabled so the result is a static binary that runs on the minimal
# runtime image with no libc compatibility concerns.

# ---- Go build ----
FROM --platform=$BUILDPLATFORM golang:1.26.9-bookworm AS build

ARG APP_VERSION=dev
ARG APP_COMMIT=unknown
ARG APP_BUILT_AT=unknown
ARG TARGETOS
ARG TARGETARCH

# The proxy is configurable because a direct fetch from the module proxy can be
# slow or blocked depending on the network.
ARG GOPROXY=https://goproxy.cn,direct

WORKDIR /src

# Copy the module files first so the dependency layer is cached independently of
# the source: editing a .go file does not invalidate the module download.
COPY go.mod go.sum ./
RUN GOFLAGS=-mod=mod GOPROXY=${GOPROXY} go mod download

# web/dist is committed, so the image build needs no Node toolchain: embed
# picks up the console files as they are. (When developing the console, run
# `make web` first to refresh internal/api/console from web/src.)
COPY internal internal
COPY cmd cmd
COPY scripts/collect-go-licenses.go scripts/collect-go-licenses.go

# -trimpath removes the build directory from the binary, which keeps paths out
# of panic traces and makes the build reproducible across machines.
# -s -w drop the symbol table and DWARF data, which nothing in production needs.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/relaypool/relaypool/internal/buildinfo.Version=${APP_VERSION} \
        -X github.com/relaypool/relaypool/internal/buildinfo.Commit=${APP_COMMIT} \
        -X github.com/relaypool/relaypool/internal/buildinfo.BuiltAt=${APP_BUILT_AT}" \
      -o /out/relaypool ./cmd/server

# 从本次实际工具链与依赖收集许可，避免镜像遗漏声明或沿用其他构建的版本。
RUN go run ./scripts/collect-go-licenses.go /out/licenses/go

# ---- runtime ----
FROM alpine:3.22

# ca-certificates is required for the outbound TLS calls to the providers.
# tzdata is required because check-in scheduling is expressed in LOCAL hours;
# without it the container has no zone database and falls back to UTC, which
# would fire the daily job at the wrong time.
# 基础标签可能来自旧缓存；先升级系统包，避免保留已修复的 OpenSSL 漏洞。
RUN apk upgrade --no-cache \
    && apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 relaypool \
    && adduser -S -D -H -u 10001 -G relaypool relaypool \
    && mkdir -p /data /run/relaypool \
    && chown -R relaypool:relaypool /data /run/relaypool

WORKDIR /app
COPY --from=build /out/relaypool /app/relaypool
# The armor presets are DATA, not code: they are read at runtime from
# armor.dir (default "prompts", relative to the working directory). Without
# this copy the image starts with zero presets, so every opt-in silently
# reports "none" -- which reads like a client bug rather than a missing file.
COPY prompts /app/prompts
# 发行包保留订阅适配所参考的 MIT 许可证与署名。
COPY internal/provider/subscriptions/NOTICE.md internal/provider/subscriptions/LICENSE-*.txt /app/licenses/subscriptions/
COPY LICENSE NOTICE.md /app/licenses/
COPY licenses/web /app/licenses/web/
COPY licenses/protocols /app/licenses/protocols/
COPY --from=build /out/licenses/go /app/licenses/go/

USER relaypool

# Defaults are the safe ones: loopback-only, data under /data.
ENV RELAYPOOL_LISTEN=0.0.0.0:8787 \
    RELAYPOOL_DATA_DIR=/data \
    RELAYPOOL_RUNTIME_DIR=/run/relaypool \
    TZ=Asia/Shanghai

# EXPOSE is documentation only; the compose file decides the host binding.
EXPOSE 8787

# The healthcheck hits the unauthenticated probe, so it needs no credential.
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8787/healthz || exit 1

ENTRYPOINT ["/app/relaypool"]
