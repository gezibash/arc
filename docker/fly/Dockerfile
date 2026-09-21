# A relay and the DM provider in one image, for one Fly.io machine.
#
# This file builds everything from this checkout, so a remote builder needs no
# other image:
#   fly deploy -c docker/fly/fly.toml --dockerfile docker/fly/Dockerfile
FROM golang:1.27-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/ ./cmd/...

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 arc \
    && mkdir -p /home/arc/.config/arc /home/arc/.arc \
    && chown -R arc:arc /home/arc

ENV HOME=/home/arc \
    PATH=/app:$PATH

COPY --from=build --chown=arc:arc /out /app
COPY --chown=arc:arc cmd/dm-provider/Arcfile cmd/dm-provider/manifest.json /opt/arc-providers/dm/
COPY --chmod=755 docker/local/run-provider.sh /opt/arc-providers/dm/run.sh
COPY --chmod=755 docker/fly/entrypoint.sh /opt/arc-fly/entrypoint.sh

USER arc
WORKDIR /home/arc
VOLUME ["/home/arc/.config/arc"]
EXPOSE 7331

ENTRYPOINT ["/opt/arc-fly/entrypoint.sh"]
