#!/bin/sh
# Entry point for the TAG gateway container. Bootstraps managed state (idempotent)
# then serves the OpenAI-compatible gateway on 0.0.0.0:$PORT.
#
# Env:
#   PORT                    bind port (default 8787; set 7860 for Hugging Face Spaces)
#   TAG_PROVIDER            default provider for prefixless models (default echo)
#   TAG_GATEWAY_KEY         bearer token required from clients (read by `tag gateway`)
#   TAG_ALLOW_UNAUTHENTICATED=1   accept unauthenticated requests when no key is set (INSECURE)
#   OPENAI_API_KEY / ANTHROPIC_API_KEY / TAG_LOCAL_BASE_URL / EXA_API_KEY  provider config
set -e
: "${PORT:=8787}"
: "${TAG_PROVIDER:=echo}"

tag bootstrap >/dev/null 2>&1 || true

AUTH=""
if [ -z "$TAG_GATEWAY_KEY" ]; then
  if [ "$TAG_ALLOW_UNAUTHENTICATED" = "1" ]; then
    AUTH="--allow-unauthenticated"
    echo "WARNING: no TAG_GATEWAY_KEY set — serving UNAUTHENTICATED (TAG_ALLOW_UNAUTHENTICATED=1)."
  else
    echo "ERROR: set TAG_GATEWAY_KEY, or TAG_ALLOW_UNAUTHENTICATED=1 to serve without auth." >&2
    exit 1
  fi
fi

# TAG_GATEWAY_KEY is read from the environment by `tag gateway` itself.
exec tag gateway --host 0.0.0.0 --port "$PORT" --provider "$TAG_PROVIDER" --fallback $AUTH
