#!/usr/bin/env bash
# Install or upgrade Sentinel on this server in one step:
#
#   git clone https://github.com/weeklyvillain/sentinel.git && sentinel/deploy/setup.sh
#
# It checks Docker, asks for the domain (first run only), starts Sentinel
# behind Caddy with automatic HTTPS, and prints the address and the one-time
# setup code to finish in the browser. Run it again to upgrade: it pulls the
# latest code and rebuilds; your data and settings are kept.
set -euo pipefail
cd "$(dirname "$0")"

b=$(tput bold 2>/dev/null || true); r=$(tput sgr0 2>/dev/null || true)
step() { printf '\n%s==> %s%s\n' "$b" "$1" "$r"; }
fail() { printf '\n%sError:%s %s\n' "$b" "$r" "$1" >&2; exit 1; }

step "Checking Docker"
command -v docker >/dev/null || fail "Docker is not installed. On Linux: curl -fsSL https://get.docker.com | sh"
docker compose version >/dev/null 2>&1 || fail "The Docker Compose plugin is missing."
docker info >/dev/null 2>&1 || fail "Cannot reach Docker. Is it running, and are you root or in the docker group?"
echo "    Docker $(docker version --format '{{.Server.Version}}'), Compose $(docker compose version --short)"

if [ -f .env ] && grep -q '^SENTINEL_DOMAIN=' .env; then
	DOMAIN=$(sed -n 's/^SENTINEL_DOMAIN=//p' .env | tail -1)
	step "Upgrading the Sentinel at $DOMAIN"
	if git -C .. rev-parse --git-dir >/dev/null 2>&1; then git -C .. pull --ff-only; fi
else
	step "Domain"
	echo "    Sentinel needs a DNS name pointing at this server (an A/AAAA record),"
	echo "    and ports 80 and 443 open. Caddy gets the HTTPS certificate itself."
	while :; do
		read -r -p "    Domain, e.g. sentinel.example.com: " DOMAIN
		DOMAIN=${DOMAIN#https://}; DOMAIN=${DOMAIN#http://}; DOMAIN=${DOMAIN%%/*}
		[[ "$DOMAIN" =~ ^[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]] && break
		echo "    That does not look like a domain name."
	done
	if command -v getent >/dev/null && ! getent hosts "$DOMAIN" >/dev/null; then
		echo "    ${b}Note:${r} $DOMAIN does not resolve yet. HTTPS starts working once the DNS record exists."
	fi
	if [ -f .env ]; then echo "SENTINEL_DOMAIN=$DOMAIN" >> .env; else
		sed "s/^SENTINEL_DOMAIN=.*/SENTINEL_DOMAIN=$DOMAIN/" .env.example > .env
	fi
	chmod 600 .env
fi

step "Starting Sentinel (the first build takes a few minutes)"
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
docker compose up -d --build

step "Waiting for Sentinel"
code=""
for _ in $(seq 1 60); do
	if [ "$(docker inspect -f '{{.State.Health.Status}}' "$(docker compose ps -q sentinel)" 2>/dev/null)" = healthy ]; then
		code=$(docker compose logs --no-color --since "$started" sentinel 2>/dev/null | sed -n 's/.*enter the setup code \([^ ]*\).*/\1/p' | tail -1)
		break
	fi
	sleep 3
done

echo
if [ -n "$code" ]; then
	cat <<EOF
${b}Sentinel is running.${r} Finish in the browser:

    1. Open        ${b}https://$DOMAIN${r}
    2. Enter code  ${b}$code${r}
    3. Click "Create GitHub App", install it, and sign in.

EOF
else
	cat <<EOF
${b}Sentinel is running${r} at ${b}https://$DOMAIN${r}.
Not set up yet? The setup code is in: docker compose logs sentinel | grep 'setup code'

EOF
fi
echo "Upgrade later by running this script again."
