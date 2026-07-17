.PHONY: help dev build web test test-pg lint clean docker user docs

VERSION ?= dev
# 8080 is a crowded port on a developer's machine (it is also the default for half the
# world's app servers), so the dev port is overridable: make dev PORT=9000
PORT ?= 8099

help:
	@echo "make build          build the SPA, then the binary with it embedded"
	@echo "make user           create a local admin account (prompts for a password)"
	@echo "make dev            run the server on :$(PORT) against the local Docker socket"
	@echo "make test           go test (SQLite only)"
	@echo "make test-pg        go test against BOTH dialects (spins a throwaway Postgres)"
	@echo "make lint           go vet + vue-tsc"
	@echo "make docker         build the container image"
	@echo "make docs           run the documentation site (VitePress) with hot reload"
	@echo
	@echo "Override the port with: make dev PORT=9000"

web:
	cd web && pnpm install && pnpm build

# The docs site is a standalone VitePress project under site/. Its dev script runs the asset
# prebuild first (sourcing the OpenAPI spec and logo), so this is the whole command.
docs:
	cd site && pnpm install && pnpm dev

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/daffa ./cmd/daffa
	@echo "→ bin/daffa"

# Create the first account. Separate target because `dev` should be re-runnable without
# tripping over an account that already exists.
user: build
	DAFFA_DATA_DIR=./.data ./bin/daffa user add -u admin --role Admin

# Runs against the local daemon. The cookie relaxes because dev is http://localhost and
# a Secure cookie would simply be dropped by the browser.
dev: build
	DAFFA_DATA_DIR=./.data DAFFA_SECURE_COOKIE=false DAFFA_ADDR=:$(PORT) ./bin/daffa serve

test:
	go test ./...

# The dual-dialect promise is only worth anything if both are actually exercised.
test-pg:
	@docker rm -f daffa-test-pg >/dev/null 2>&1 || true
	@docker run -d --rm --name daffa-test-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=daffa \
		-p 55432:5432 postgres:17-alpine >/dev/null
	@until docker exec daffa-test-pg pg_isready -U postgres >/dev/null 2>&1; do sleep 0.5; done
	-DAFFA_TEST_PG_URL="postgres://postgres:test@localhost:55432/daffa?sslmode=disable" go test ./...
	@docker rm -f daffa-test-pg >/dev/null

lint:
	go vet ./...
	cd web && pnpm exec vue-tsc --noEmit

docker:
	docker build -t daffa:$(VERSION) .

clean:
	rm -rf bin .data internal/web/dist
