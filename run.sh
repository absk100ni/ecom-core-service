#!/usr/bin/env bash
# Start the backend with a chosen environment file.
#   ./run.sh          -> dev  (.env)
#   ./run.sh prod     -> prod-like local run (.env.prod)
set -euo pipefail
cd "$(dirname "$0")"

ENV_NAME="${1:-dev}"
case "$ENV_NAME" in
  dev)  ENV_FILE=".env" ;;
  prod) ENV_FILE=".env.prod" ;;
  *) echo "usage: ./run.sh [dev|prod]" >&2; exit 1 ;;
esac

if [[ ! -f "$ENV_FILE" ]]; then
  echo "ERROR: $ENV_FILE not found" >&2
  exit 1
fi

# Guardrail: prod mode touches the REAL ecom_prod database and real S3.
if [[ "$ENV_NAME" == "prod" ]]; then
  echo "⚠️  PROD MODE: this run uses the real ecom_prod database and real S3 bucket."
  read -r -p "Type 'prod' to continue: " CONFIRM
  [[ "$CONFIRM" == "prod" ]] || { echo "aborted"; exit 1; }
fi

set -a            # export everything the env file defines
source "./$ENV_FILE"
set +a

echo "==> starting backend with $ENV_FILE (ENVIRONMENT=${ENVIRONMENT:-unset}, MONGO_DB=${MONGO_DB:-unset})"
exec go run ./cmd/api
