#!/bin/sh
set -eu

if [ -x /usr/local/bin/plex-gateway ]; then
    exec /usr/local/bin/plex-gateway cache-reset "$@"
fi

service=${PLEX_GATEWAY_COMPOSE_SERVICE:-plex-gateway}
exec docker compose exec -T "$service" /usr/local/bin/plex-gateway-reset-mediainfo-cache "$@"
