# Deploying the TAG gateway

`tag gateway` serves the native agent loop as an OpenAI-compatible API
(`POST /v1/chat/completions` streaming + non-stream, `GET /v1/models`,
`GET /health`) behind a bearer token. This directory packages it for one-push
hosting — any OpenAI client (TypingMind, opencode, the `openai` SDK, `curl`) can
then point its base URL at your deployment.

## Docker (local or any host)

```sh
# from tag-go/
docker build -t tag-gateway .
docker run -p 8787:8787 \
  -e TAG_GATEWAY_KEY=$(openssl rand -hex 16) \
  -e TAG_PROVIDER=openai \
  -e OPENAI_API_KEY=sk-... \
  tag-gateway
```

Then:

```sh
curl http://localhost:8787/v1/chat/completions \
  -H "Authorization: Bearer $TAG_GATEWAY_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
```

`--fallback` is on in the container entrypoint, so once you configure a
`route-fallback` chain (e.g. a `local/` last-resort model) the gateway walks it
when a provider fails. Out of the box no chain is configured, so nothing fails
over until you add one. Point `TAG_LOCAL_BASE_URL` at a sidecar llama.cpp/ollama
server to give that chain a local last resort.

## Render (render.com)

Commit the repo-root `render.yaml` and create a Blueprint (Render auto-detects
it only at the repo root), or point a new Docker web service at
`tag-go/Dockerfile` with `tag-go/` as the build context. `TAG_GATEWAY_KEY` is
auto-generated; set your
provider keys (`OPENAI_API_KEY`, …) as secret env vars in the dashboard. Render
health-checks `/health`.

## Hugging Face Spaces (Docker SDK, free tier)

HF Spaces serves on port **7860**. Create a Docker Space, add this repo's
`tag-go/` as the build context (or copy the `Dockerfile`), and set:

- `PORT=7860`
- `TAG_GATEWAY_KEY` = a secret token (Space → Settings → Secrets)
- `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` / `EXA_API_KEY` as secrets

The Space rebuilds on push. To serve without auth for a quick demo, set
`TAG_ALLOW_UNAUTHENTICATED=1` (INSECURE — anyone can spend your provider keys).

## Keep-warm (free tiers idle out)

Free tiers stop the container after inactivity. Add an
[UptimeRobot](https://uptimerobot.com) HTTP monitor hitting
`https://<your-deploy>/health` every 5 minutes to keep it warm. `/health`
requires no auth and is cheap.

## Environment reference

| Variable | Purpose |
|---|---|
| `PORT` | bind port (8787 default; 7860 for HF Spaces) |
| `TAG_PROVIDER` | default provider for prefixless models (`echo`/`openai`/`anthropic`/`local`) |
| `TAG_GATEWAY_KEY` | bearer token clients must present |
| `TAG_ALLOW_UNAUTHENTICATED` | `1` to serve without a key (INSECURE) |
| `OPENAI_API_KEY`, `ANTHROPIC_API_KEY` | cloud provider keys |
| `TAG_LOCAL_BASE_URL` | a local OpenAI-compatible server (llama.cpp/ollama) for the `local` provider |
| `EXA_API_KEY` | enables the `web_search` tool |
