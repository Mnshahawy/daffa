# Three stages, one artifact: the SPA is compiled by Node, baked into the Go binary,
# and the final image carries neither Node nor a shell. What ships is a single static
# executable and its CA bundle.
#
# Multi-arch without emulation: both build stages pin to $BUILDPLATFORM so they run
# natively on the runner (never under QEMU), and the Go stage cross-compiles to the
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

# ── 2. build the binary (native builder, cross-compiled to the target arch) ─────────
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=web /src/internal/web/dist ./internal/web/dist

# TARGETOS/TARGETARCH are supplied per target platform by buildx. CGO off (pure-Go SQLite
# driver) is what lets GOARCH cross-compilation produce a static binary with no C toolchain
# and no emulation.
ARG TARGETOS TARGETARCH
ENV CGO_ENABLED=0
ARG VERSION=dev
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/daffa ./cmd/daffa

# Stage an empty data dir to copy into the final image with nonroot ownership. Distroless
# has no shell to mkdir/chown in place, so the directory is built here and copied below.
RUN mkdir -p /out/state/var/lib/daffa

# ── 3. ship ─────────────────────────────────────────────────────────────────────
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
