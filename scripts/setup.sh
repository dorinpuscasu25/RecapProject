#!/usr/bin/env bash
# Creates .env from .env.example with freshly generated secrets (never overwrites an existing .env).
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ -f .env ]]; then echo ".env already exists; leaving it unchanged"; exit 0; fi
rand() { openssl rand -base64 32 2>/dev/null || head -c 32 /dev/urandom | base64; }
cp .env.example .env
sed -i.bak \
  -e "s|^SECRETS_ENCRYPTION_KEY=.*|SECRETS_ENCRYPTION_KEY=$(rand)|" \
  -e "s|^AUTH_CODE_HMAC_SECRET=.*|AUTH_CODE_HMAC_SECRET=$(rand)|" \
  -e "s|^AUTH_SIGNING_KEYS=.*|AUTH_SIGNING_KEYS=k1:$(rand)|" \
  -e "s|^AUTH_ACTIVE_KEY_ID=.*|AUTH_ACTIVE_KEY_ID=k1|" \
  -e "s|^CRAWL_LIVE_ENABLED=.*|CRAWL_LIVE_ENABLED=true|" .env
# A first administrator for local use; change the address and password before sharing the stack.
admin_pw="Adm1n-$(rand | tr -dc 'A-Za-z0-9' | head -c 16)"
demo_pw="Rec4p-$(rand | tr -dc 'A-Za-z0-9' | head -c 10)"
sed -i.bak \
  -e "s|^ADMIN_EMAIL=.*|ADMIN_EMAIL=${ADMIN_EMAIL:-admin@example.com}|" \
  -e "s|^ADMIN_PASSWORD=.*|ADMIN_PASSWORD=${ADMIN_PASSWORD:-$admin_pw}|" \
  -e "s|^ADMIN_PHONE=.*|ADMIN_PHONE=${ADMIN_PHONE:-+37369000000}|" \
  -e "s|^DEMO_EMAIL=.*|DEMO_EMAIL=${DEMO_EMAIL:-demo@example.com}|" \
  -e "s|^DEMO_PASSWORD=.*|DEMO_PASSWORD=${DEMO_PASSWORD:-$demo_pw}|" \
  -e "s|^DEMO_PHONE=.*|DEMO_PHONE=${DEMO_PHONE:-+37369000001}|" .env
rm -f .env.bak
echo "created .env with generated secrets."
echo "admin login: $(grep ^ADMIN_EMAIL= .env | cut -d= -f2) / password in .env (ADMIN_PASSWORD)"
echo "demo user:   $(grep ^DEMO_EMAIL= .env | cut -d= -f2) / $(grep ^DEMO_PASSWORD= .env | cut -d= -f2)   (for the apps and the web console)"
echo "web console: http://localhost:8080/console/   email inbox: http://localhost:8025"
echo "next: optionally add API keys (ANTHROPIC_API_KEY / OPENAI_API_KEY / GEMINI_API_KEY), then: docker compose up -d"
