# A Nostr relay for ARC, for one Fly.io machine. See docs/delivery/SPEC.md,
# section 15.1, step 2.
#
# Deploy from the root of this checkout:
#   fly deploy -c docker/fly-nostr/fly.toml --dockerfile docker/fly-nostr/Dockerfile \
#     --build-arg VERSION=X.Y.Z
FROM golang:1.27-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/arcn ./cmd/arcn

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 arc \
    && mkdir -p /data \
    && chown arc:arc /data

COPY --from=build /out/arcn /app/arcn

USER arc
VOLUME ["/data"]
EXPOSE 8080

# The relay keeps its events in /data/relay.db, on the volume. Fly ends TLS,
# and sends X-Forwarded-Proto, so NIP-42 authentication expects wss://.
#
# The write limits, docs/delivery/SPEC.md section 12:
# - An event holds at most 256 KiB. A journal part holds 32 KiB of text.
# - A gift wrap needs NIP-42 authentication, or NIP-13 work of 20 bits.
# - One IP address writes 300 events each minute, and 1000 at once. The Fly
#   proxy puts the address of the client in Fly-Client-IP.
# - The store stops at 800 MiB, on a volume of 1 GB.
ENTRYPOINT ["/app/arcn", "--home", "/data", "relay", "serve", "--listen", "0.0.0.0:8080", "--db", "/data/relay.db", \
    "--max-event-bytes", "262144", "--wrap-auth", "--wrap-pow", "20", \
    "--rate", "300", "--burst", "1000", "--ip-header", "Fly-Client-IP", "--max-store-mb", "800"]
