# syntax=docker/dockerfile:1

# 1. Dashboard (React) -> internal/web/dist
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN mkdir -p /src/internal/web && npm run build

# 2. Go binary (pure Go, no cgo)
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/web/dist ./internal/web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sentinel ./cmd/sentinel \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sentinel-demo ./cmd/sentinel-demo \
 && mkdir -p /out/data /out/fixer-auth

# 3. Minimal runtime: static binary, CA certificates, non-root user
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sentinel /usr/local/bin/sentinel
# Demo traffic generator, used by deploy/docker-compose.demo.yml.
COPY --from=build /out/sentinel-demo /usr/local/bin/sentinel-demo
COPY --from=build --chown=nonroot:nonroot /out/data /data
# Shared with the fixer container: Sentinel writes the fixer's token here.
COPY --from=build --chown=nonroot:nonroot /out/fixer-auth /fixer-auth
ENV SENTINEL_DB=/data/sentinel.db \
    SENTINEL_OTLP_ADDR=:4318 \
    SENTINEL_HTTP_ADDR=:8080
EXPOSE 4318 8080
VOLUME /data
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD ["/usr/local/bin/sentinel", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/sentinel"]
