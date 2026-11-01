# syntax=docker/dockerfile:1
FROM node:lts-alpine AS frontend
WORKDIR /src/frontend
RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then export NODE_EXTRA_CA_CERTS=/run/secrets/proxy_ca; fi; \
    npm install --global pnpm@12.16.1
COPY frontend/package.json frontend/pnpm-lock.yaml frontend/pnpm-workspace.yaml ./
RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then export NODE_EXTRA_CA_CERTS=/run/secrets/proxy_ca; fi; \
    pnpm install --frozen-lockfile
COPY frontend/ ./
RUN pnpm build

FROM golang:alpine AS backend
WORKDIR /src
RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then export SSL_CERT_FILE=/run/secrets/proxy_ca; fi; \
    apk add --no-cache build-base
COPY go.mod go.sum ./
RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then export SSL_CERT_FILE=/run/secrets/proxy_ca; fi; \
    go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /cloudpilot ./cmd/cloudpilot

FROM alpine:latest
RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then export SSL_CERT_FILE=/run/secrets/proxy_ca; fi; \
    apk add --no-cache ca-certificates docker-cli tzdata
WORKDIR /app
COPY --from=backend /cloudpilot /usr/local/bin/cloudpilot
COPY --from=frontend /src/frontend/dist ./frontend/dist
COPY config.example.yaml ./config.example.yaml
RUN mkdir -p /app/data/runs
EXPOSE 8080 9090
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -Y off -q -O /dev/null http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["cloudpilot"]
CMD ["-config", "/app/config.yaml"]
