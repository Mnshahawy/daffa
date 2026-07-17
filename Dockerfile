# Three stages, one artifact: the SPA is compiled by Node, baked into the Go binary,
# and the final image carries neither Node nor a shell. What ships is a single static
# executable and its CA bundle.

# ── 1. build the SPA ────────────────────────────────────────────────────────────
FROM node:22-alpine AS web
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

# ── 2. build the binary (with the SPA embedded) ─────────────────────────────────
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=web /src/internal/web/dist ./internal/web/dist

# CGO off keeps this a static binary — hence the pure-Go SQLite driver.
ENV CGO_ENABLED=0
ARG VERSION=dev
RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/daffa ./cmd/daffa

# ── 3. ship ─────────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/daffa /usr/local/bin/daffa

# Daffa needs to read the Docker socket, which is root-owned; the compose file grants
# that with group_add rather than by running this as root.
USER nonroot:nonroot
VOLUME /var/lib/daffa
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/daffa"]
CMD ["serve"]
