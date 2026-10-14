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
  -e "s|^AUTH_ACTIVE_KEY_ID=.*|AUTH_ACTIVE_KEY_ID=k1|" .env
rm -f .env.bak
echo "created .env with generated secrets."
echo "next: add API keys (ANTHROPIC_API_KEY / OPENAI_API_KEY / GEMINI_API_KEY) and ADMIN_* if wanted, then: docker compose up -d"
