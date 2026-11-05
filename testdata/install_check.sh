#!/bin/sh
# Checks install.sh's decisions on each platform, using stand-ins for uname, curl and
# docker. Real Compose then renders the result, but nothing is built or started, and
# no network is used. This can't show that Docker Desktop itself behaves as expected:
# install.sh tests that on the real machine.
#
#   sh testdata/install_check.sh [shell]    # default sh; try dash, "busybox sh", bash

set -u
SHELL_CMD=${1:-sh}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
REAL_DOCKER=$(command -v docker || true)
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
failed=0

check() {
	if [ "$2" = "$3" ]; then echo "PASS  $1"; else
		echo "FAIL  $1: want [$3] got [$2]"
		failed=$((failed + 1))
	fi
}
has() { if grep -q -- "$2" "$1" 2>/dev/null; then echo yes; else echo no; fi; }

# The source archive, built from the working tree.
mkdir "$T/src" "$T/src/claudia-x" "$T/bin"
cp "$ROOT"/compose.yaml "$ROOT"/compose.https.yaml "$T/src/claudia-x/"
: >"$T/src/claudia-x/main.go"
tar -czf "$T/source.tgz" -C "$T/src" claudia-x

# A PATH with only these tools, so docker can really be missing.
for tool in sh dash busybox bash cat sed grep tr head tar gzip mkdir rm mv mktemp ls id sleep; do
	real=$(command -v "$tool") && ln -s "$real" "$T/bin/$tool"
done
cat >"$T/bin/uname" <<'EOF'
#!/bin/sh
if [ "${1:-}" = -m ]; then echo x86_64; else echo "$STUB_OS"; fi
EOF
cat >"$T/bin/curl" <<EOF
#!/bin/sh
for a in "\$@"; do
	case \$a in
	*api.github.com*) echo '  "tag_name": "v9.9.9",'; exit 0 ;;
	*archive/*) while [ "\$1" != -o ]; do shift; done; cat "$T/source.tgz" >"\$2"; exit 0 ;;
	esac
done
exit 0
EOF
cat >"$T/bin/docker" <<'EOF'
#!/bin/sh
echo "docker $*" >>"$STUB_LOG"
case "$1 ${2:-}" in
"info --format") echo "$STUB_ENGINE" ;;
"run --rm")
	case "$*" in *"--entrypoint curl"*) printf '%s' "$STUB_MCP" ;; esac ;;
esac
exit 0
EOF
chmod +x "$T/bin/uname" "$T/bin/curl" "$T/bin/docker"

# install OS ENGINE MCP_CODE DIR [args]: runs the installer, keeping its output.
install() {
	os=$1 engine=$2 code=$3 dir=$4
	shift 4
	PATH="$T/bin" HOME="$T" STUB_OS=$os STUB_ENGINE=$engine STUB_MCP=$code STUB_LOG="$T/docker.log" \
		$SHELL_CMD "$ROOT/install.sh" --dir "$dir" "$@" >"$T/out" 2>&1
}
# rendered DIR: what real Compose makes of the files install.sh left behind.
rendered() {
	(cd "$1" && "$REAL_DOCKER" compose config 2>&1)
}
ports() {
	rendered "$1" | sed -nE 's/^ *(published|host_ip): *"?([^"]*)"?$/\2/p' | tr '\n' ' '
}

echo "== native Linux"
D=$T/linux
install Linux "Debian GNU/Linux" 401 "$D"
check "installs" $? 0
check "no desktop file" "$(ls "$D" | grep -c desktop)" 0
check "only the base compose file" "$(has "$D/.env" "^COMPOSE_FILE='compose.yaml'$")" yes
check ".env is private" "$(ls -l "$D/.env" | cut -c1-10)" "-rw-------"
PW=$(sed -n "s/^CLAUDIA_PASSWORD='\(.*\)'$/\1/p" "$D/.env")
check "generated a 32 character password" "${#PW}" 32
check "shows it once" "$(has "$T/out" "Password: $PW")" yes
check "tells a server how to reach it" "$(has "$T/out" "ssh -L 8080:127.0.0.1:8080")" yes
check "no warnings" "$(has "$T/out" warning:)" no
check "used the newest release" "$(has "$T/out" "Downloading Claudia v9.9.9")" yes
if [ -n "$REAL_DOCKER" ]; then
	check "Compose: agents' port on the gateway" "$(rendered "$D" | grep -c 'host_ip: 10.77.0.1')" 1
	check "Compose: reads the password from .env" "$(rendered "$D" | grep -c "CLAUDIA_PASSWORD: $PW")" 1
fi

echo "== default folder"
PATH="$T/bin" HOME="$T/home" STUB_OS=Linux STUB_ENGINE=Debian STUB_MCP=401 STUB_LOG="$T/docker.log" \
	$SHELL_CMD "$ROOT/install.sh" >"$T/out" 2>&1
check "installs under the home folder" "$(has "$T/out" "Settings: $T/home/.claudia/.env")" yes

echo "== upgrade"
echo "CLAUDIA_AGENT_MEMORY='8g'" >>"$D/.env"
echo stale >"$D/removed_upstream.go"
install Linux "Debian GNU/Linux" 401 "$D" --port 9090
check "upgrades" $? 0
check "keeps the password" "$(has "$D/.env" "^CLAUDIA_PASSWORD='$PW'$")" yes
check "doesn't show it again" "$(has "$T/out" "Password: unchanged")" yes
check "keeps the owner's settings" "$(has "$D/.env" "^CLAUDIA_AGENT_MEMORY='8g'$")" yes
check "applies the new port" "$(has "$D/.env" "^CLAUDIA_PORT='9090'$")" yes
check "no duplicate settings" "$(grep -c '^CLAUDIA_PASSWORD=' "$D/.env")" 1
check "drops source files removed upstream" "$(ls "$D" | grep -c removed_upstream)" 0
install Linux "Debian GNU/Linux" 401 "$D"
check "remembers the port" "$(has "$D/.env" "^CLAUDIA_PORT='9090'$")" yes

echo "== Docker Desktop"
for platform in "Darwin" "MINGW64_NT-10.0-22631" "Linux"; do
	D=$T/desktop-$platform
	install "$platform" "Docker Desktop" 401 "$D"
	check "$platform: installs" $? 0
	check "$platform: uses the desktop file" "$(has "$D/.env" "^COMPOSE_FILE='compose.yaml:compose.desktop.yaml'$")" yes
	check "$platform: probes through the host" "$(has "$T/docker.log" "http://host.docker.internal:8081/mcp")" yes
	check "$platform: no tunnel advice" "$(has "$T/out" "ssh -L")" no
	if [ -n "$REAL_DOCKER" ]; then
		check "$platform: Compose: nothing published on the VM's gateway" "$(rendered "$D" | grep -c 'host_ip: 10.77.0.1')" 0
		check "$platform: Compose: approval port stays on loopback" "$(ports "$D")" "127.0.0.1 8080 127.0.0.1 8081 "
		check "$platform: Compose: agents told the host's address" "$(rendered "$D" | grep -c 'CLAUDIA_MCP_URL: http://host.docker.internal:8081/mcp')" 1
	fi
	: >"$T/docker.log"
done
check "Windows gets its path conversion" "$(has "$T/desktop-MINGW64_NT-10.0-22631/.env" COMPOSE_CONVERT_WINDOWS_PATHS)" yes
check "macOS doesn't" "$(has "$T/desktop-Darwin/.env" COMPOSE_CONVERT_WINDOWS_PATHS)" no

echo "== HTTPS"
D=$T/https
install Linux "Ubuntu 24.04" 401 "$D" --domain farm.example.com
check "installs" $? 0
check "adds the https file" "$(has "$D/.env" "^COMPOSE_FILE='compose.yaml:compose.https.yaml'$")" yes
check "uses the domain" "$(has "$T/out" "Open:     https://farm.example.com")" yes
D=$T/https-desktop
install Darwin "Docker Desktop" 401 "$D" --domain farm.example.com
if [ -n "$REAL_DOCKER" ]; then
	check "desktop override keeps ports 80 and 443" "$(rendered "$D" | grep -cE 'published: "(80|443)"')" 2
fi

echo "== refusals"
mkdir "$T/project"
echo keep >"$T/project/mine.go"
install Linux "Debian GNU/Linux" 401 "$T/project"
check "refuses a folder with other files" $? 1
check "leaves them alone" "$(cat "$T/project/mine.go")" keep
install Linux x 401 "$T/a" --port '80;x'
check "refuses a bad port" $? 1
install Linux x 401 "$T/a" --domain "x'y"
check "refuses a bad domain" $? 1
install Linux x 401 "$T/a" --version 'v1;rm'
check "refuses a bad version" $? 1
install SunOS x 401 "$T/a"
check "refuses other systems" $? 1
export CLAUDIA_PASSWORD="it's"
install Linux x 401 "$T/a"
check "refuses a password that breaks .env" $? 1
unset CLAUDIA_PASSWORD

echo "== docker missing"
mv "$T/bin/docker" "$T/docker.off"
install Darwin x 401 "$T/a"
check "macOS: stops" $? 1
check "macOS: points to Docker Desktop" "$(has "$T/out" docker-desktop)" yes
install Linux x 401 "$T/a"
check "Linux: stops" $? 1
check "Linux: offers the flag" "$(has "$T/out" "--install-docker")" yes
mv "$T/docker.off" "$T/bin/docker"

echo "== problems found after starting"
install Linux "Debian GNU/Linux" 000 "$T/noapproval"
check "still finishes" $? 0
check "warns about ask-first" "$(has "$T/out" "ask-first mode won't work")" yes

echo
if [ "$failed" -eq 0 ]; then echo "all passed"; else
	echo "$failed failed"
	exit 1
fi
