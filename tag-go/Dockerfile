# syntax=docker/dockerfile:1
#
# Turnkey deploy image for the TAG OpenAI-compatible gateway (gap #7 from the
# hermes-octo parity review). Multi-stage: build the CGO-free static binary,
# then run `tag gateway` in a minimal, non-root Alpine runtime. See deploy/ for
# Render, Hugging Face Spaces, and keep-warm instructions.

FROM golang:1.25-alpine AS build
WORKDIR /src
# Cache module downloads on an unchanged go.mod/go.sum.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Static, stripped binary — no libc dependency, so it runs on scratch/alpine.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tag ./cmd/tag

FROM alpine:3.20
# ca-certificates for TLS to cloud providers; wget (busybox) powers HEALTHCHECK.
RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10001 tag
COPY --from=build /out/tag /usr/local/bin/tag
COPY deploy/entrypoint.sh /usr/local/bin/tag-entrypoint
RUN chmod +x /usr/local/bin/tag-entrypoint

USER tag
WORKDIR /home/tag
ENV TAG_HOME=/home/tag/.tag \
    PORT=8787 \
    TAG_PROVIDER=echo
EXPOSE 8787

# The gateway exposes GET /health; wget is present via busybox in alpine.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/tag-entrypoint"]
