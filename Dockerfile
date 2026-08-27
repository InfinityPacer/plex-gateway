# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY VERSION ./
COPY cmd/plex-gateway ./cmd/plex-gateway
COPY internal ./internal
RUN release_version="$(tr -d '[:space:]' < VERSION)" \
    && CGO_ENABLED=0 go build -trimpath \
        -ldflags="-s -w -X github.com/InfinityPacer/plex-gateway/internal/version.release=${release_version}" \
        -o /plex-gateway ./cmd/plex-gateway

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && adduser -D -H -s /sbin/nologin gateway
COPY --from=build /plex-gateway /usr/local/bin/plex-gateway
USER gateway
EXPOSE 32400
ENV LISTEN_ADDR=:32400
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 CMD ["/usr/local/bin/plex-gateway", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/plex-gateway"]
