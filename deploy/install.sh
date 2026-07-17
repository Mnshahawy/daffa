#!/usr/bin/env bash
#
# Daffa installer — one command to a running console.
#
#   curl -fsSL https://raw.githubusercontent.com/mnshahawy/daffa/main/deploy/install.sh | sudo bash
#
# or, non-interactively (automation):
#
#   sudo ./install.sh --no-prompts --domain daffa.example.com --acme-email you@example.com
#
# What it does: installs Docker if it is missing, writes /opt/daffa/{docker-compose.yml,.env}
# with a bundled PostgreSQL, brings the stack up, and creates the first admin account.
#
# It is idempotent: run it again to pull a newer image and restart. It will not
# regenerate secrets or recreate the admin on an existing install.
#
# Two modes, chosen by whether you give a --domain:
#   • with --domain   → Traefik terminates TLS on :443 with a Let's Encrypt cert.
#   • without         → Daffa on http://127.0.0.1:8080, no proxy (local / bring-your-own-edge).
#
set -euo pipefail

# ── defaults ─────────────────────────────────────────────────────────────────
INSTALL_DIR="${DAFFA_INSTALL_DIR:-/opt/daffa}"
# owner/name on GitHub. The releases API and raw.githubusercontent are both
# case-insensitive here. Override for a fork: DAFFA_REPO=you/daffa
REPO="${DAFFA_REPO:-Mnshahawy/daffa}"
# GHCR requires a lowercase image path; derive it so a fork stays in sync with REPO.
IMAGE_REPO="ghcr.io/$(printf '%s' "$REPO" | tr '[:upper:]' '[:lower:]')"
# Left empty so the resolved release tag drives the image; --image / DAFFA_IMAGE override.
DAFFA_IMAGE="${DAFFA_IMAGE:-}"
# Empty = install the latest release; --version pins a specific tag (e.g. v1.2.3).
VERSION=""
DOMAIN=""
ACME_EMAIL=""
ADMIN_USER="admin"
ADMIN_PASSWORD=""
BIND="127.0.0.1"
PORT="8080"
NO_PROMPTS=0
FORCE_TRAEFIK=""   # "", "1" (--traefik), or "0" (--no-traefik) — "" means infer from --domain
FORCE_INTERNAL=""  # "", "1" (--internal), or "0" (--public) — "" means ask / default public

# ── pretty output ────────────────────────────────────────────────────────────
if [ -t 1 ]; then
  B=$'\033[1m'; DIM=$'\033[2m'; RED=$'\033[31m'; GRN=$'\033[32m'; YLW=$'\033[33m'; RST=$'\033[0m'
else
  B=""; DIM=""; RED=""; GRN=""; YLW=""; RST=""
fi
say()  { printf '%s==>%s %s\n' "$B" "$RST" "$*"; }
ok()   { printf '%s ✓ %s%s\n' "$GRN" "$*" "$RST"; }
warn() { printf '%s ! %s%s\n' "$YLW" "$*" "$RST" >&2; }
die()  { printf '%s ✗ %s%s\n' "$RED" "$*" "$RST" >&2; exit 1; }

usage() {
  cat <<'EOF'
Daffa installer

Usage: install.sh [options]

  --domain DOMAIN        DNS name for TLS mode; Traefik gets a Let's Encrypt cert for it.
                         Omit for direct localhost mode (plain http, no proxy).
  --acme-email EMAIL     Contact address for Let's Encrypt (required in public TLS mode).
  --internal / --public  Internal domain (Daffa issues the cert from its own CA and prints a
                         trust bundle) vs public (Let's Encrypt). Prompted if neither is given.
  --admin-user NAME      First admin username (default: admin).
  --admin-password PASS  First admin password (>= 12 chars; generated if omitted).
  --traefik / --no-traefik
                         Force the mode instead of inferring it from --domain.
  --bind ADDR            Direct-mode publish address (default: 127.0.0.1).
  --port PORT            Direct-mode publish port (default: 8080).
  --version TAG          Install a specific release (e.g. v1.2.3) instead of the latest.
                         Selects the compose file, the image tag, and the GitHub ref together.
  --image REF            Full image ref, overriding the release-tag default.
  --install-dir DIR      Where to write compose + .env (default: /opt/daffa).
  --no-prompts           Never prompt; rely on flags only. Missing required values are errors.
  -h, --help             This help.

Values can also come from the environment: DAFFA_INSTALL_DIR, DAFFA_IMAGE, DAFFA_REPO.
EOF
}

# ── flags ────────────────────────────────────────────────────────────────────
while [ $# -gt 0 ]; do
  case "$1" in
    --domain)          DOMAIN="${2:-}"; shift 2 ;;
    --acme-email)      ACME_EMAIL="${2:-}"; shift 2 ;;
    --admin-user)      ADMIN_USER="${2:-}"; shift 2 ;;
    --admin-password)  ADMIN_PASSWORD="${2:-}"; shift 2 ;;
    --traefik)         FORCE_TRAEFIK=1; shift ;;
    --no-traefik)      FORCE_TRAEFIK=0; shift ;;
    --internal)        FORCE_INTERNAL=1; shift ;;
    --public)          FORCE_INTERNAL=0; shift ;;
    --bind)            BIND="${2:-}"; shift 2 ;;
    --port)            PORT="${2:-}"; shift 2 ;;
    --version)         VERSION="${2:-}"; shift 2 ;;
    --image)           DAFFA_IMAGE="${2:-}"; shift 2 ;;
    --install-dir)     INSTALL_DIR="${2:-}"; shift 2 ;;
    --no-prompts)      NO_PROMPTS=1; shift ;;
    -h|--help)         usage; exit 0 ;;
    *)                 die "unknown option: $1 (try --help)" ;;
  esac
done

# ── prompt helpers (honour --no-prompts) ─────────────────────────────────────
# ask VAR "Question" "default" — reads into the named variable, or falls back to
# the default under --no-prompts / no tty.
ask() {
  local __var="$1" __q="$2" __def="${3:-}" __reply=""
  if [ "$NO_PROMPTS" = 1 ] || [ ! -t 0 ]; then
    printf -v "$__var" '%s' "$__def"; return
  fi
  if [ -n "$__def" ]; then
    read -r -p "$__q [$__def]: " __reply || true
    [ -z "$__reply" ] && __reply="$__def"
  else
    read -r -p "$__q: " __reply || true
  fi
  printf -v "$__var" '%s' "$__reply"
}

need_root() {
  if [ "$(id -u)" -ne 0 ]; then
    die "this needs root (it installs Docker and writes ${INSTALL_DIR}). Re-run with sudo."
  fi
}

rand_secret() {
  # 24 url-safe-ish chars; enough entropy for a db password and >= 12 for the admin.
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -base64 24 | tr -d '/+=' | cut -c1-24
  else
    head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' | cut -c1-24
  fi
}

# Fetch a URL to stdout, failing (non-zero) on any transport or HTTP error, so
# callers can `|| die`. curl or wget — whichever the box has.
http_get() {
  if command -v curl >/dev/null 2>&1; then curl -fsSL "$1"
  elif command -v wget >/dev/null 2>&1; then wget -qO- "$1"
  else die "need curl or wget"; fi
}

install_docker() {
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    ok "Docker present ($(docker --version | awk '{print $3}' | tr -d ,))"
    return
  fi
  if command -v docker >/dev/null 2>&1; then
    warn "docker is installed but the daemon isn't reachable — trying to start it"
    systemctl start docker 2>/dev/null || service docker start 2>/dev/null || true
    docker info >/dev/null 2>&1 && { ok "Docker daemon started"; return; }
  fi
  say "installing Docker via get.docker.com"
  curl -fsSL https://get.docker.com | sh || die "Docker install failed"
  systemctl enable --now docker 2>/dev/null || service docker start 2>/dev/null || true
  docker info >/dev/null 2>&1 || die "Docker installed but the daemon is not running"
  ok "Docker installed"
}

# `docker compose` (v2 plugin) or the legacy `docker-compose` binary, always run
# from INSTALL_DIR so the neighbouring .env is auto-loaded — that is how
# COMPOSE_PROFILES (traefik vs direct) reaches Compose.
compose() {
  if docker compose version >/dev/null 2>&1; then
    ( cd "$INSTALL_DIR" && docker compose "$@" )
  elif command -v docker-compose >/dev/null 2>&1; then
    ( cd "$INSTALL_DIR" && docker-compose "$@" )
  else
    die "neither 'docker compose' nor 'docker-compose' is available"
  fi
}

docker_gid() {
  getent group docker 2>/dev/null | cut -d: -f3 | grep -q . && { getent group docker | cut -d: -f3; return; }
  # No docker group (rootless, or an odd distro): fall back to the socket's owner gid.
  stat -c '%g' /var/run/docker.sock 2>/dev/null || echo 999
}

# Resolve the release to install: an explicit --version wins, otherwise ask GitHub
# for the latest published release. Sets TAG, COMPOSE_REF (the git ref to fetch the
# compose file from) and the default image. Falls back to main/:latest with a warning
# when there is no release yet, so a first-run-before-first-release still works.
resolve_release() {
  if [ -n "$VERSION" ]; then
    TAG="$VERSION"
  else
    say "resolving the latest Daffa release"
    TAG="$(http_get "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
      | grep -m1 '"tag_name"' \
      | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"
  fi
  if [ -n "$TAG" ]; then
    COMPOSE_REF="$TAG"
    # The release workflow pushes the git tag verbatim as an image tag, so the tag
    # is the one identifier for the ref, the compose file, and the image.
    DAFFA_IMAGE="${DAFFA_IMAGE:-${IMAGE_REPO}:${TAG}}"
    ok "release ${TAG}"
  else
    warn "no published release found — using 'main' and :latest (pin one with --version vX.Y.Z)"
    TAG="latest"; COMPOSE_REF="main"
    DAFFA_IMAGE="${DAFFA_IMAGE:-${IMAGE_REPO}:latest}"
  fi
}

# Download the compose file for the resolved ref. The repo is the single source of
# truth for the stack shape — the installer no longer carries its own copy.
fetch_compose() {
  local base="https://raw.githubusercontent.com/${REPO}/${COMPOSE_REF}/deploy"
  say "fetching compose file (${COMPOSE_REF})"
  http_get "$base/docker-compose.yml" > "$INSTALL_DIR/docker-compose.yml" 2>/dev/null \
    || die "could not download the compose file: $base/docker-compose.yml"
  # Guard against a 404 body or an HTML error page landing in the file.
  grep -q '^name: daffa' "$INSTALL_DIR/docker-compose.yml" \
    || die "downloaded compose file doesn't look right — aborting rather than run it"

  # Under Traefik, the compose mounts ./traefik.yml — one config that serves both public and
  # internal sites (per-router, no global resolver). The ACME resolver is appended only when
  # we have an email: always in a public install, optionally in an internal one (to let other
  # public sites on this host use Let's Encrypt).
  if [ "$USE_TRAEFIK" = 1 ]; then
    say "fetching Traefik config"
    http_get "$base/traefik.yml" > "$INSTALL_DIR/traefik.yml" 2>/dev/null \
      || die "could not download the Traefik config: $base/traefik.yml"
    grep -q '^entryPoints:' "$INSTALL_DIR/traefik.yml" \
      || die "downloaded Traefik config doesn't look right — aborting"
    if [ -n "$ACME_EMAIL" ]; then
      # Traefik does not expand env vars in its static config, so append a concrete block.
      cat >> "$INSTALL_DIR/traefik.yml" <<EOF

certificatesResolvers:
  le:
    acme:
      email: "${ACME_EMAIL}"
      storage: /acme/acme.json
      tlsChallenge: {}
EOF
    fi
  fi
}

# ── main ─────────────────────────────────────────────────────────────────────
say "Daffa installer"
need_root

# We fetch the compose file and query the releases API, so one of these is required
# up front (install_docker also needs curl to reach get.docker.com).
command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 \
  || die "this needs curl or wget installed"

# Decide the mode. --traefik/--no-traefik win; otherwise a --domain (or a prompted
# one) means TLS, its absence means direct.
if [ -z "$FORCE_TRAEFIK" ] && [ -z "$DOMAIN" ]; then
  ask DOMAIN "Domain for HTTPS (blank = localhost-only, no TLS)" ""
fi
case "$FORCE_TRAEFIK" in
  1) USE_TRAEFIK=1 ;;
  0) USE_TRAEFIK=0 ;;
  *) [ -n "$DOMAIN" ] && USE_TRAEFIK=1 || USE_TRAEFIK=0 ;;
esac

# Daffa refuses passwords under 12 chars; catch a too-short --admin-password here
# rather than let it surface later as a confusing "account may already exist".
if [ -n "$ADMIN_PASSWORD" ] && [ "${#ADMIN_PASSWORD}" -lt 12 ]; then
  die "--admin-password must be at least 12 characters"
fi

# These networks and volumes are the console's own plumbing. Daffa marks them `system`,
# so no one can delete them from inside the console. daffa-edge-certs / traefik-acme only
# exist under Traefik, so they are added to the volume list only then.
SYSTEM_NETWORKS="daffa-internal,daffa-edge"
SYSTEM_VOLUMES="daffa-data,daffa-pg"

INTERNAL=0
CERTRESOLVER=""   # DAFFA_CERTRESOLVER for the daffa router: `le` (public) or empty (internal)
if [ "$USE_TRAEFIK" = 1 ]; then
  [ -n "$DOMAIN" ] || die "TLS mode needs a domain (--domain), which resolves to this host"
  PROFILES="traefik"; SECURE_COOKIE="true"; TRUST_PROXY="true"
  ACCESS_URL="https://$DOMAIN"
  SYSTEM_VOLUMES="$SYSTEM_VOLUMES,daffa-edge-certs,traefik-acme,daffa-traefik-config"

  # Public (Let's Encrypt) or internal (Daffa's own CA) — for THIS console. Traefik itself
  # serves both kinds of site regardless; this only picks the console's own certificate. A
  # public domain must be reachable from the internet for the ACME challenge; an internal
  # one is not, so Daffa issues the certificate and we hand back a trust bundle.
  case "$FORCE_INTERNAL" in
    1) INTERNAL=1 ;;
    0) INTERNAL=0 ;;
    *) if [ "$NO_PROMPTS" = 1 ] || [ ! -t 0 ]; then
         INTERNAL=0   # default to public/ACME under automation
       else
         ans=""; read -r -p "Is ${DOMAIN} reachable from the public internet (for Let's Encrypt)? [Y/n]: " ans || true
         case "$ans" in [Nn]*) INTERNAL=1 ;; *) INTERNAL=0 ;; esac
       fi ;;
  esac

  if [ "$INTERNAL" = 1 ]; then
    CERTRESOLVER=""   # the console's cert is Daffa-delivered, not ACME
    say "internal mode: Daffa will issue the certificate for ${DOMAIN} from its own CA"
    # An email is optional here: it enables the ACME resolver so OTHER, public sites behind
    # this same Traefik can still get Let's Encrypt certs. The console does not use it.
    [ -n "$ACME_EMAIL" ] && say "ACME will also be configured (for public sites on this host)"
  else
    CERTRESOLVER="le"
    [ -n "$ACME_EMAIL" ] || ask ACME_EMAIL "Email for Let's Encrypt (expiry notices)" ""
    [ -n "$ACME_EMAIL" ] || die "public TLS mode needs --acme-email (or use --internal)"
  fi
else
  PROFILES=""; SECURE_COOKIE="false"; TRUST_PROXY="false"
  ACCESS_URL="http://${BIND}:${PORT}"
  warn "direct mode: plain http on ${BIND}:${PORT}, no TLS. Fine for localhost; put a"
  warn "TLS proxy in front (or re-run with --domain) before exposing it to a network."
fi

install_docker
resolve_release
DGID="$(docker_gid)"
mkdir -p "$INSTALL_DIR"

ENV_FILE="$INSTALL_DIR/.env"
FRESH=1
if [ -f "$ENV_FILE" ]; then
  FRESH=0
  say "existing install at $INSTALL_DIR — upgrading (keeping .env and secrets)"
  # Reuse the stored db password so we don't lock ourselves out of the volume.
  # shellcheck disable=SC1090
  POSTGRES_PASSWORD="$(grep -E '^POSTGRES_PASSWORD=' "$ENV_FILE" | head -1 | cut -d= -f2-)"
else
  POSTGRES_PASSWORD="$(rand_secret)"
fi

fetch_compose

# .env is written every run so mode/domain/image changes take effect on upgrade,
# but the db password and (on a fresh install) admin password are the only secrets.
umask 077
cat > "$ENV_FILE" <<EOF
# Written by install.sh — safe to edit, then: docker compose up -d
COMPOSE_PROFILES=$PROFILES
DAFFA_IMAGE=$DAFFA_IMAGE
DAFFA_DOMAIN=$DOMAIN
ACME_EMAIL=$ACME_EMAIL
DAFFA_CERTRESOLVER=$CERTRESOLVER
DAFFA_SECURE_COOKIE=$SECURE_COOKIE
DAFFA_TRUST_PROXY=$TRUST_PROXY
DAFFA_BIND=$BIND
DAFFA_PORT=$PORT
DOCKER_GID=$DGID
DAFFA_CONFIG_DIR=$INSTALL_DIR
DAFFA_SYSTEM_NETWORKS=$SYSTEM_NETWORKS
DAFFA_SYSTEM_VOLUMES=$SYSTEM_VOLUMES
POSTGRES_PASSWORD=$POSTGRES_PASSWORD
EOF
chmod 600 "$ENV_FILE"
ok "wrote $INSTALL_DIR/{docker-compose.yml,.env}$([ "$USE_TRAEFIK" = 1 ] && echo ',traefik.yml')"

say "pulling images"
compose pull

# Traefik reads its static config once at startup, so the config volume must hold traefik.yml
# before Traefik's first start. Seed it via a throwaway container that writes through the live
# mount (a copy into a stopped container would be shadowed when the volume mounts over it —
# the same trick Daffa's own delivery uses). `daffa stack config` takes over management after.
if [ "$USE_TRAEFIK" = 1 ]; then
  say "seeding the Traefik config volume"
  docker volume create daffa-traefik-config >/dev/null
  docker run --rm -v daffa-traefik-config:/dst -v "$INSTALL_DIR":/src:ro busybox \
    sh -c 'cp /src/traefik.yml /dst/traefik.yml' \
    || die "could not seed the Traefik config volume — check that Docker can pull busybox"
fi

say "starting Daffa"
compose up -d

# Wait for the server to answer /healthz. It only listens after migrations run, so
# this doubles as "the schema is ready" — which is when `user add` is safe. We poll
# the loopback publish, which exists in both modes.
say "waiting for Daffa to come up"
HEALTH_URL="http://127.0.0.1:${PORT}/healthz"
up=0
for _ in $(seq 1 60); do
  if http_get "$HEALTH_URL" >/dev/null 2>&1; then up=1; break; fi
  sleep 2
done
[ "$up" = 1 ] || die "Daffa did not become healthy in time — check: (cd $INSTALL_DIR && docker compose logs daffa)"
ok "Daffa is up"

# Create the first admin — only on a fresh install; on upgrade the account exists.
CREATED_ADMIN=0
if [ "$FRESH" = 1 ]; then
  [ -n "$ADMIN_PASSWORD" ] || { ADMIN_PASSWORD="$(rand_secret)"; GEN_ADMIN_PW=1; }
  # user add reads the password twice from stdin (Password + Confirm) when not on a
  # tty, and needs the full binary path since `docker exec` bypasses the entrypoint.
  if add_out="$(printf '%s\n%s\n' "$ADMIN_PASSWORD" "$ADMIN_PASSWORD" \
      | compose exec -T daffa \
        /usr/local/bin/daffa user add -u "$ADMIN_USER" --role Admin 2>&1)"; then
    CREATED_ADMIN=1
    ok "created admin account '$ADMIN_USER'"
  else
    # Show what daffa actually said (already-exists, short password, …) rather than guess.
    warn "could not create the admin account automatically:"
    [ -n "$add_out" ] && warn "  $add_out"
    warn "create one manually with:"
    warn "  cd $INSTALL_DIR && docker compose exec daffa /usr/local/bin/daffa user add -u admin --role Admin"
  fi
fi

# Internal mode: issue the edge certificate from Daffa's own CA and capture the trust
# bundle. Idempotent — safe on re-run — so it runs on every internal-mode install, not
# just the first. stdout is the bundle PEM (logs go to stderr), so it redirects cleanly.
EDGE_BUNDLE=""
if [ "$INTERNAL" = 1 ]; then
  say "issuing the internal edge certificate for ${DOMAIN}"
  if compose exec -T daffa /usr/local/bin/daffa edge init \
        --domain "$DOMAIN" --volume daffa-edge-certs > "$INSTALL_DIR/ca-bundle.crt" 2>/tmp/daffa-edge.err; then
    chmod 644 "$INSTALL_DIR/ca-bundle.crt"
    EDGE_BUNDLE="$INSTALL_DIR/ca-bundle.crt"
    ok "issued edge certificate; CA trust bundle saved to $EDGE_BUNDLE"
  else
    warn "could not issue the internal certificate:"
    [ -s /tmp/daffa-edge.err ] && warn "  $(tail -1 /tmp/daffa-edge.err)"
    warn "retry with: cd $INSTALL_DIR && docker compose exec daffa /usr/local/bin/daffa edge init --domain $DOMAIN --volume daffa-edge-certs"
    rm -f "$INSTALL_DIR/ca-bundle.crt"
  fi
  rm -f /tmp/daffa-edge.err
fi

# Register the running deployment as an editable Daffa stack, so its own features (drift,
# redeploy, env editing) apply — and so the domain can be changed from the console and
# redeployed. Idempotent; reads the compose + .env from the install dir mounted at /etc/daffa.
# -u 0: the .env is root-owned mode 600 and Daffa runs as non-root, so this one-shot reads
# it as root. No new exposure — the container already holds the DB password (DAFFA_DB_URL)
# and the Docker socket. adopt writes only to the database, nothing to the data volume.
say "registering the deployment as a Daffa stack"
if compose exec -u 0 -T daffa /usr/local/bin/daffa stack adopt --name daffa >/dev/null 2>/tmp/daffa-adopt.err; then
  ok "registered stack 'daffa' (edit the domain in the console, then redeploy)"
else
  warn "could not register the stack automatically:"
  [ -s /tmp/daffa-adopt.err ] && warn "  $(tail -1 /tmp/daffa-adopt.err)"
  warn "retry with: cd $INSTALL_DIR && docker compose exec -u 0 daffa /usr/local/bin/daffa stack adopt"
fi
rm -f /tmp/daffa-adopt.err

# Put the Traefik config under management too, so traefik.yml and the dynamic middlewares
# directory are editable in the console and re-delivered on each deploy. Reads traefik.yml
# from the mounted install dir (644, so no -u 0 needed).
if [ "$USE_TRAEFIK" = 1 ]; then
  say "placing Traefik config under Daffa management"
  if compose exec -T daffa /usr/local/bin/daffa stack config >/dev/null 2>/tmp/daffa-cfg.err; then
    ok "traefik.yml and the dynamic config are now editable in the console (Volume sources)"
  else
    warn "could not place Traefik config under management:"
    [ -s /tmp/daffa-cfg.err ] && warn "  $(tail -1 /tmp/daffa-cfg.err)"
    warn "retry with: cd $INSTALL_DIR && docker compose exec daffa /usr/local/bin/daffa stack config"
  fi
  rm -f /tmp/daffa-cfg.err
fi

# ── summary ──────────────────────────────────────────────────────────────────
echo
ok "Daffa is running."
echo
printf '  %sURL%s        %s\n' "$B" "$RST" "$ACCESS_URL"
printf '  %sVersion%s    %s\n' "$DIM" "$RST" "$TAG"
if [ "$USE_TRAEFIK" = 1 ] && [ "$INTERNAL" = 0 ]; then
  printf '  %sDNS%s        point %s at this host; a Let'\''s Encrypt cert is issued on first request to :443\n' "$DIM" "$RST" "$DOMAIN"
fi
if [ "$INTERNAL" = 1 ]; then
  printf '  %sDNS%s        make %s resolve to this host (internal DNS or /etc/hosts)\n' "$DIM" "$RST" "$DOMAIN"
fi
if [ "$CREATED_ADMIN" = 1 ]; then
  printf '  %sUsername%s   %s\n' "$B" "$RST" "$ADMIN_USER"
  if [ "${GEN_ADMIN_PW:-0}" = 1 ]; then
    printf '  %sPassword%s   %s   %s(generated — save it now)%s\n' "$B" "$RST" "$ADMIN_PASSWORD" "$YLW" "$RST"
  else
    printf '  %sPassword%s   (the one you passed)\n' "$B" "$RST"
  fi
fi
if [ -n "$EDGE_BUNDLE" ]; then
  echo
  printf '  %sTrust%s      the certificate is signed by Daffa'\''s internal CA. Install the bundle on\n' "$B" "$RST"
  printf '             every machine that opens the console, or browsers will warn:\n'
  printf '               %s%s%s\n' "$DIM" "$EDGE_BUNDLE" "$RST"
  printf '             Linux: copy to /usr/local/share/ca-certificates/daffa.crt && update-ca-certificates\n'
  printf '             macOS: security add-trusted-cert -d -k /Library/Keychains/System.keychain %s\n' "$EDGE_BUNDLE"
fi
echo
printf '  %sConfig%s     %s\n' "$DIM" "$RST" "$INSTALL_DIR/.env"
printf '  %sManage%s     cd %s && docker compose {ps,logs -f,down,up -d}\n' "$DIM" "$RST" "$INSTALL_DIR"
echo
