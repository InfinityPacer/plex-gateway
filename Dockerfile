# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY VERSION ./
COPY cmd/plex-gateway ./cmd/plex-gateway
COPY internal ./internal
RUN release_version="$(tr -d '[:space:]' < VERSION)" \
    && CGO_ENABLED=0 go build -trimpath \
        -ldflags="-s -w -X github.com/InfinityPacer/plex-gateway/internal/version.release=${release_version}" \
        -o /plex-gateway ./cmd/plex-gateway

FROM alpine:3.22
RUN apk add --no-cache ca-certificates ffmpeg \
    && addgroup -g 1000 gateway \
    && adduser -D -H -u 1000 -G gateway -s /sbin/nologin gateway \
    && install -d -o gateway -g gateway /app_data
COPY --from=build /plex-gateway /usr/local/bin/plex-gateway
COPY --chmod=755 scripts/reset-mediainfo-cache.sh /usr/local/bin/plex-gateway-reset-mediainfo-cache
WORKDIR /app_data
USER gateway
EXPOSE 32400
ENV LISTEN_ADDR=:32400
ENV DATABASE_PATH=/app_data/plex-gateway.db
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 CMD ["/usr/local/bin/plex-gateway", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/plex-gateway"]
