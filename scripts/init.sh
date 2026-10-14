#!/bin/sh
# Compose init step (runs inside the backend image). Idempotent.
set -eu
echo "== migrations"; migrate
echo "== seed";       NEWSCTL_QUIET=1 newsctl seed
if [ -n "${ADMIN_EMAIL:-}" ] && [ -n "${ADMIN_PASSWORD:-}" ] && [ -n "${ADMIN_PHONE:-}" ]; then
  echo "== admin";    NEWSCTL_QUIET=1 newsctl create-admin
else
  echo "== admin: skipped (set ADMIN_EMAIL, ADMIN_PASSWORD, ADMIN_PHONE in .env to create one)"
fi
if [ -n "${DEMO_EMAIL:-}" ] && [ -n "${DEMO_PASSWORD:-}" ] && [ -n "${DEMO_PHONE:-}" ]; then
  echo "== demo user";  NEWSCTL_QUIET=1 newsctl create-user
fi
if [ "${SEED_FIXTURES:-false}" = "true" ]; then
  echo "== synthetic fixture sources (processed by the worker)"; NEWSCTL_QUIET=1 newsctl ingest-fixtures -drain=false
fi
echo "== init done"
