#!/bin/sh
# Installs or upgrades Claudia with Docker, on Linux, macOS or Windows.
#
#   curl -fsSL https://raw.githubusercontent.com/alexcolls/claudia/main/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --domain farm.example.com --install-docker
#
#   --dir DIR         where to install (default ~/.claudia)
#   --port PORT       local port for the web UI (default 8080)
#   --domain DOMAIN   serve HTTPS with Let's Encrypt. Needs DNS and ports 80 and 443
#   --version REF     tag or branch to install (default: the newest release)
#   --install-docker  on Linux, install Docker through get.docker.com if it's missing
#
# Set CLAUDIA_PASSWORD to choose the login password. Otherwise one is generated.
# On Windows, run this from Git Bash or WSL with Docker Desktop running.
#
# Everything is inside main, so a download cut off halfway runs nothing.

set -eu

REPO=alexcolls/claudia

warn() { printf 'warning: %s\n' "$*" >&2; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

dk() {
	if [ -n "$use_sudo" ]; then sudo docker "$@"; else docker "$@"; fi
}

# A setting from the existing .env, so upgrades keep it.
saved() {
	if [ -f "$dir/.env" ]; then
		sed -n "s/^$1='\{0,1\}\([^']*\)'\{0,1\}\$/\1/p" "$dir/.env"
	fi
}

need_docker() {
	command -v docker >/dev/null 2>&1 && return 0
	case $os in
	Linux)
		[ -n "$install_docker" ] || die "Docker isn't installed. Run this again with --install-docker, or see https://docs.docker.com/engine/install/"
		if [ "$(id -u)" = 0 ]; then
			curl -fsSL https://get.docker.com | sh
		else
			curl -fsSL https://get.docker.com | sudo sh
		fi
		command -v docker >/dev/null 2>&1 || die "Docker's installer didn't finish. See https://docs.docker.com/engine/install/"
		;;
	*)
		die "Docker isn't available. Install and start Docker Desktop, then run this again: https://www.docker.com/products/docker-desktop/"
		;;
	esac
}

find_docker() {
	use_sudo=
	if ! docker info >/dev/null 2>&1; then
		if [ "$os" = Linux ] && command -v sudo >/dev/null 2>&1 && sudo docker info >/dev/null 2>&1; then
			use_sudo=1
		else
			die "Docker is installed but isn't responding. Start it, then run this again."
		fi
	fi
	dk compose version >/dev/null 2>&1 || die "Docker Compose v2 is missing: https://docs.docker.com/compose/install/"
}

# Downloads first, so a failed download leaves an existing install untouched.
fetch_source() {
	if [ -z "$version" ]; then
		# Not /releases/latest, which skips pre-releases.
		version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases?per_page=1" |
			sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)
		[ -n "$version" ] || die "Couldn't find the newest release. Choose one with --version."
	fi
	case $version in *[!A-Za-z0-9._/-]*) die "Invalid version: $version" ;; esac

	tmp=$(mktemp)
	trap 'rm -f "$tmp"' EXIT
	echo "Downloading Claudia $version"
	curl -fsSL -o "$tmp" "https://github.com/$REPO/archive/$version.tar.gz" || die "Couldn't download version $version."
	mkdir -p "$dir"
	rm -f "$dir"/*.go # a file dropped upstream would otherwise still get compiled
	tar -xzf "$tmp" --strip-components=1 -C "$dir"
	: >"$dir/.claudia-install"
}

# Docker Desktop keeps 10.77.0.1 inside its VM, so the host can't publish a port on it.
# Agents reach the approval endpoint through the host instead.
write_desktop_override() {
	cat >"$dir/compose.desktop.yaml" <<'EOF'
# Written by install.sh for Docker Desktop.
services:
  claudia:
    environment:
      CLAUDIA_MCP_URL: http://host.docker.internal:8081/mcp
    ports: !override
      - "${CLAUDIA_BIND:-127.0.0.1}:${CLAUDIA_PORT:-8080}:8080"
      - "127.0.0.1:8081:8081"
EOF
}

write_env() {
	files=compose.yaml
	if [ -n "$desktop" ]; then
		write_desktop_override
		files=$files:compose.desktop.yaml
	fi
	# After the desktop file, whose !override would otherwise drop these ports.
	if [ -n "$domain" ]; then files=$files:compose.https.yaml; fi

	# Keeps anything else the owner has set, such as CLAUDIA_AGENT_MEMORY.
	kept=
	if [ -f "$dir/.env" ]; then
		kept=$(grep -vE '^(CLAUDIA_PASSWORD|CLAUDIA_PORT|CLAUDIA_DOMAIN|COMPOSE_FILE|COMPOSE_PATH_SEPARATOR|COMPOSE_CONVERT_WINDOWS_PATHS)=' "$dir/.env" || true)
	fi
	(
		umask 077
		{
			if [ -n "$kept" ]; then printf '%s\n' "$kept"; fi
			printf "CLAUDIA_PASSWORD='%s'\n" "$password"
			printf "CLAUDIA_PORT='%s'\n" "$port"
			if [ -n "$domain" ]; then printf "CLAUDIA_DOMAIN='%s'\n" "$domain"; fi
			printf "COMPOSE_FILE='%s'\n" "$files"
			echo "COMPOSE_PATH_SEPARATOR=':'"
			case $os in MINGW* | MSYS* | CYGWIN*) echo "COMPOSE_CONVERT_WINDOWS_PATHS='1'" ;; esac
		} >"$dir/.env.new"
	)
	mv "$dir/.env.new" "$dir/.env"
}

wait_for() {
	i=0
	until curl -fs -o /dev/null -m 3 "$1"; do
		i=$((i + 1))
		[ "$i" -lt 60 ] || return 1
		sleep 1
	done
}

# These can't be known in advance on every machine, so each install tests them.
check_install() {
	mcp=http://10.77.0.1:8081/mcp
	if [ -n "$desktop" ]; then mcp=http://host.docker.internal:8081/mcp; fi
	code=$(dk run --rm --network claudia-agents --entrypoint curl claudia-agent:latest \
		-s -o /dev/null -m 10 -w '%{http_code}' "$mcp" || true)
	# 401 means it arrived: the probe sends no agent token.
	[ "$code" = 401 ] || warn "Agents can't reach the approval endpoint at $mcp, so ask-first mode won't work. Other modes aren't affected."

	for cli in claude codex agy; do
		dk run --rm --entrypoint "$cli" claudia-agent:latest --version >/dev/null 2>&1 ||
			warn "$cli won't start on this machine ($(uname -m)), so agents using it will fail. Other CLIs aren't affected."
	done
}

main() {
	dir=${CLAUDIA_DIR:-${HOME:?HOME isn\'t set: use CLAUDIA_DIR}/.claudia}
	port='' domain='' version='' install_docker=''
	while [ $# -gt 0 ]; do
		case $1 in
		--dir | --port | --domain | --version)
			[ $# -ge 2 ] || die "$1 needs a value"
			case $1 in
			--dir) dir=$2 ;;
			--port) port=$2 ;;
			--domain) domain=$2 ;;
			--version) version=$2 ;;
			esac
			shift 2
			;;
		--install-docker)
			install_docker=1
			shift
			;;
		-h | --help)
			sed -n '2,15s/^# \{0,1\}//p' "$0" 2>/dev/null || echo "See https://github.com/$REPO"
			return 0
			;;
		*) die "Unknown option: $1" ;;
		esac
	done

	# These end up in .env, so they can't be allowed to break out of its quoting.
	case $port in *[!0-9]*) die "Invalid port: $port" ;; esac
	case $domain in *[!A-Za-z0-9.-]*) die "Invalid domain: $domain" ;; esac
	case ${CLAUDIA_PASSWORD:-} in *\'* | *"
"*) die "CLAUDIA_PASSWORD can't contain a single quote or a line break." ;; esac

	os=$(uname -s)
	case $os in
	Linux | Darwin | MINGW* | MSYS* | CYGWIN*) ;;
	*) die "$os isn't supported. Claudia installs on Linux, macOS and Windows." ;;
	esac
	command -v curl >/dev/null 2>&1 || die "curl is required."
	command -v tar >/dev/null 2>&1 || die "tar is required."

	# Checked before anything is written, so a wrong --dir can't scatter files.
	if [ -d "$dir" ] && [ ! -f "$dir/.claudia-install" ] && [ -n "$(ls -A "$dir")" ]; then
		die "$dir has other files in it. Choose an empty folder with --dir."
	fi

	need_docker
	find_docker

	# WSL reports Linux, but can still be using Docker Desktop.
	desktop=
	case $os in Linux) ;; *) desktop=1 ;; esac
	case $(dk info --format '{{.OperatingSystem}}') in *"Docker Desktop"*) desktop=1 ;; esac

	fetch_source
	dir=$(cd "$dir" && pwd)

	port=${port:-$(saved CLAUDIA_PORT)}
	port=${port:-8080}
	domain=${domain:-$(saved CLAUDIA_DOMAIN)}
	password=${CLAUDIA_PASSWORD:-$(saved CLAUDIA_PASSWORD)}
	new_password=
	if [ -z "$password" ]; then
		password=$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32)
		new_password=1
	fi
	write_env

	cd "$dir"
	echo "Building images. The first install takes several minutes."
	dk compose up -d --build

	if [ -n "$domain" ]; then url=https://$domain; else url=http://127.0.0.1:$port; fi
	if ! wait_for "$url"; then
		[ -n "$domain" ] || die "Claudia didn't answer at $url. See: cd '$dir' && docker compose logs claudia"
		warn "$url isn't answering yet. Check that DNS points here and that ports 80 and 443 are open."
	fi
	check_install

	echo
	echo "Claudia is running."
	echo "  Open:     $url"
	if [ -n "$new_password" ]; then
		echo "  Password: $password"
	else
		echo "  Password: unchanged"
	fi
	echo "  Settings: $dir/.env"
	if [ -z "$domain" ] && [ -z "$desktop" ]; then
		echo
		echo "On a remote server, the UI stays private. Reach it through a tunnel from your own machine:"
		echo "  ssh -L $port:127.0.0.1:$port you@this-server"
	fi
	echo
	echo "Before real work, read \"Do these before running real work\" in README.md."
	echo "To upgrade, run this installer again."
}

main "$@"
