#!/bin/sh
# Daffa node provisioning: install Docker (and the Compose plugin) on a machine so Daffa can manage
# it over the Docker socket. This is the ONLY script Daffa runs on a host it manages — everything
# else is the Docker API (docs/clusters.md §8, §11). It installs Docker and NOTHING more: no Traefik,
# no build tooling. Idempotent: safe on a machine that already has Docker.
set -eu

say() { printf '>> %s\n' "$*"; }
ok()  { printf 'OK %s\n' "$*"; }
die() { printf 'ERROR %s\n' "$*" >&2; exit 1; }

# Root runs commands directly; a non-root user needs passwordless sudo (docs/clusters.md §8).
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  command -v sudo >/dev/null 2>&1 || die "not running as root and sudo is not installed"
  SUDO="sudo -n"
  $SUDO true 2>/dev/null || die "not root and passwordless sudo is unavailable — connect as root or grant NOPASSWD"
fi

os_id() {
  [ -r /etc/os-release ] || return 0
  ( . /etc/os-release && printf '%s' "${ID:-}" )
}

# get.docker.com installs the Compose plugin on the distros it supports, but Amazon Linux's docker
# package ships without any Compose — fetch the plugin binary there.
ensure_compose() {
  docker compose version >/dev/null 2>&1 && return 0
  command -v docker-compose >/dev/null 2>&1 && return 0
  dest="/usr/local/lib/docker/cli-plugins"
  arch="$(uname -m)" # aarch64 / x86_64 match the release asset names verbatim
  say "installing the Docker Compose plugin (${arch})"
  $SUDO mkdir -p "$dest"
  url="https://github.com/docker/compose/releases/latest/download/docker-compose-linux-${arch}"
  if command -v curl >/dev/null 2>&1; then
    $SUDO curl -fsSL "$url" -o "$dest/docker-compose" || die "could not download the Compose plugin"
  else
    $SUDO wget -qO "$dest/docker-compose" "$url" || die "could not download the Compose plugin"
  fi
  $SUDO chmod +x "$dest/docker-compose"
  docker compose version >/dev/null 2>&1 || die "installed the Compose plugin but 'docker compose' still fails"
}

install_docker() {
  if command -v docker >/dev/null 2>&1 && $SUDO docker info >/dev/null 2>&1; then
    ok "Docker already present ($(docker --version | awk '{print $3}' | tr -d ,))"
    return 0
  fi
  if command -v docker >/dev/null 2>&1; then
    say "docker is installed but the daemon is not reachable — starting it"
    $SUDO systemctl start docker 2>/dev/null || $SUDO service docker start 2>/dev/null || true
    $SUDO docker info >/dev/null 2>&1 && { ok "Docker daemon started"; return 0; }
  fi
  # get.docker.com aborts on Amazon Linux ("Unsupported distribution 'amzn'"), so use the distro repo
  # there; every distro the convenience script supports stays on it.
  if [ "$(os_id)" = amzn ]; then
    say "installing Docker from the Amazon Linux repositories"
    $SUDO dnf install -y docker >/dev/null 2>&1 || $SUDO yum install -y docker >/dev/null 2>&1 \
      || die "Docker install failed (dnf/yum install docker)"
  else
    say "installing Docker via get.docker.com"
    if command -v curl >/dev/null 2>&1; then
      curl -fsSL https://get.docker.com | $SUDO sh || die "Docker install failed"
    elif command -v wget >/dev/null 2>&1; then
      wget -qO- https://get.docker.com | $SUDO sh || die "Docker install failed"
    else
      die "neither curl nor wget is available to fetch the Docker installer"
    fi
  fi
  $SUDO systemctl enable --now docker 2>/dev/null || $SUDO service docker start 2>/dev/null || true
  $SUDO docker info >/dev/null 2>&1 || die "Docker installed but the daemon is not running"
  ok "Docker installed"
}

# Add the connecting user to the docker group so Daffa reaches the socket without sudo on the NEXT
# connection. Root needs no group.
grant_socket() {
  [ "$(id -u)" -eq 0 ] && return 0
  user="$(id -un)"
  if id -nG "$user" | tr ' ' '\n' | grep -qx docker; then
    ok "$user is already in the docker group"
    return 0
  fi
  say "adding $user to the docker group"
  $SUDO usermod -aG docker "$user" || die "could not add $user to the docker group"
  ok "added $user to the docker group — reconnect for it to take effect"
}

say "provisioning $(hostname) — $(os_id 2>/dev/null || echo 'unknown os')"
install_docker
ensure_compose
grant_socket
ok "done — Daffa can manage this machine once you reconnect"
