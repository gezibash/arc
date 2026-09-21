# The relay and four providers in one image, for a local stack.
#
# Everything comes from this checkout:
#   docker compose -f docker/local/compose.test.yaml up
FROM golang:1.27-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/ ./cmd/...

FROM debian:bookworm-slim

# Journal uses Git. Agora uses ps to find a stale writer lock after a crash.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git procps \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 arc \
    && mkdir -p /home/arc/.config/arc /home/arc/public /home/arc/.arc/journal \
        /home/arc/.arc/dm /home/arc/.arc/agora /home/arc/releases \
        /run/arc/relay /run/arc/journal /run/arc/dm /run/arc/agora /run/arc/releases \
    && chown -R arc:arc /home/arc /run/arc

ENV HOME=/home/arc \
    PATH=/app:$PATH

COPY --from=build --chown=arc:arc /out /app

# Each provider is a bundle: a manifest, an Arcfile, and a runtime that runs
# the binary that the build stage made.
COPY cmd/journal-provider/Arcfile cmd/journal-provider/manifest.json /opt/arc-providers/journal/
COPY cmd/dm-provider/Arcfile cmd/dm-provider/manifest.json /opt/arc-providers/dm/
COPY cmd/agora-provider/Arcfile cmd/agora-provider/manifest.json /opt/arc-providers/agora/
COPY cmd/releases-provider/Arcfile cmd/releases-provider/manifest.json /opt/arc-providers/releases/
COPY --chmod=755 docker/local/run-provider.sh /opt/arc-providers/journal/run.sh
COPY --chmod=755 docker/local/run-provider.sh /opt/arc-providers/dm/run.sh
COPY --chmod=755 docker/local/run-provider.sh /opt/arc-providers/agora/run.sh
COPY --chmod=755 docker/local/run-provider.sh /opt/arc-providers/releases/run.sh
COPY --chmod=755 docker/local/entrypoint.sh docker/local/relay-health.sh /opt/arc-local/

USER arc
WORKDIR /home/arc
VOLUME ["/home/arc/.config/arc"]
EXPOSE 7331

ENTRYPOINT ["/opt/arc-local/entrypoint.sh"]
CMD ["relay"]
