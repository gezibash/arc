# Build stage: one static binary for each command.
FROM golang:1.27-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/ ./cmd/...

# Runtime stage: the binaries and the certificates, and nothing else.
FROM debian:bookworm-slim AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 arc \
    && mkdir -p /home/arc/.config/arc /home/arc/.arc \
    && chown -R arc:arc /home/arc

ENV HOME=/home/arc \
    PATH=/app:$PATH

COPY --from=build --chown=arc:arc /out /app

USER arc
WORKDIR /home/arc

# Keys, control state, and caches live under $HOME/.config/arc and
# $HOME/.arc. Mount a volume there to keep the relay identity across runs.
VOLUME ["/home/arc/.config/arc"]

EXPOSE 7331
ENTRYPOINT ["arc"]
CMD ["relay"]
