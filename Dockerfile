# Two images from one tree. The default (last) stage is the full console: the SPA is
# compiled by Node, baked into the Go binary, and shipped on a shell-less distroless base
# — a single static executable and its CA bundle. The `agent` stage is a second, much
# smaller image (`--target agent`) carrying only the dial-out agent binary: no SPA, no
# server, no DB drivers. Keeping the full stage LAST means `docker build .` and
# `make docker` still build the console; the agent is opt-in via its target.
#
# Multi-arch without emulation: the Node and Go build stages pin to $BUILDPLATFORM so they
# run natively on the runner (never under QEMU), and the Go stages cross-compile to the
# target arch. Emulating the arm64 Node+Go toolchains turned a ~1-minute build into a
# 15-minute one — the SPA output is arch-independent and the CGO-off binary cross-compiles
# cleanly, so there is nothing to gain from building either arch emulated.

# ── 1. build the SPA (once, on the native builder — its output is arch-independent) ──
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web

# Pin pnpm to 9 to match CI (.github/workflows/ci.yml). corepack's floating default
# moved to pnpm 10, whose ignored-builds gate exits non-zero on esbuild/vue-demi —
# unpinned, the image build breaks while CI stays green on 9.
RUN corepack enable && corepack prepare pnpm@9 --activate
COPY web/package.json web/pnpm-lock.yaml* ./
RUN pnpm install --frozen-lockfile || pnpm install

# The SPA's style.css imports the design tokens from the repo-root brand/ dir
# (`@import '../../brand/tokens.css'`), which resolves to /src/brand at build time;
# without this copy the CSS build can't resolve them.
COPY brand/ /src/brand/
COPY web/ ./
RUN pnpm build          # → /src/internal/web/dist

# ── 2. shared Go builder base: the module cache, downloaded once and reused by both the
#       full binary and the agent binary. Both cross-compile from $BUILDPLATFORM. ───────
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS gobase
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

# ── 2a. build the full console binary (with the SPA embedded) ───────────────────────
FROM gobase AS build
# TARGETOS/TARGETARCH are supplied per target platform by buildx; they must be re-declared
# in each stage that uses them (ARGs do not cross FROM). CGO off (pure-Go SQLite driver) is
# what lets GOARCH cross-compilation produce a static binary with no C toolchain, no emulation.
ARG TARGETOS TARGETARCH VERSION=dev
ENV CGO_ENABLED=0
COPY . .
COPY --from=web /src/internal/web/dist ./internal/web/dist
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/daffa ./cmd/daffa
# Stage an empty data dir to copy into the final image with nonroot ownership. Distroless
# has no shell to mkdir/chown in place, so the directory is built here and copied below.
RUN mkdir -p /out/state/var/lib/daffa

# ── 2b. build the agent-only binary. No SPA and no dependency on the `web` stage:
#        cmd/daffa-agent imports only internal/agent → internal/tunnel, so this needs the
#        source and nothing from Node. `--target agent` never builds the SPA. ──────────
FROM gobase AS build-agent
ARG TARGETOS TARGETARCH VERSION=dev
ENV CGO_ENABLED=0
COPY . .
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/daffa-agent ./cmd/daffa-agent
RUN mkdir -p /out/state/var/lib/daffa

# ── 3a. the agent image (~4× smaller than the console). An intermediate stage so the full
#        console below stays the default target. The agent listens on nothing — it dials
#        out — so there is no EXPOSE; it only needs its state dir to persist identity. ──
FROM gcr.io/distroless/static-debian12:nonroot AS agent
COPY --from=build-agent /out/daffa-agent /usr/local/bin/daffa-agent
# Same nonroot-owned state dir trick as the console (see the note below): the agent writes
# /var/lib/daffa/agent.json, and a root-owned mountpoint would leave nonroot unable to.
COPY --from=build-agent --chown=nonroot:nonroot /out/state/var/lib/daffa /var/lib/daffa
USER nonroot:nonroot
VOLUME /var/lib/daffa
ENTRYPOINT ["/usr/local/bin/daffa-agent"]

# ── 3. ship the full console (LAST stage = default target for `docker build .`) ─────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/daffa /usr/local/bin/daffa

# Ship /var/lib/daffa already owned by nonroot. A fresh Docker named volume inherits the
# ownership of its mountpoint in the image; if VOLUME created the directory itself it would
# be root-owned, and the nonroot process below could not write master.key — a boot crash
# loop ("writing master key: permission denied") on every first install.
COPY --from=build --chown=nonroot:nonroot /out/state/var/lib/daffa /var/lib/daffa

# Daffa needs to read the Docker socket, which is root-owned; the compose file grants
# that with group_add rather than by running this as root.
USER nonroot:nonroot
VOLUME /var/lib/daffa
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/daffa"]
CMD ["serve"]
