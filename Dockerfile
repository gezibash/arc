# Build stage: compile a prod release with the bundled Erlang runtime.
ARG ELIXIR_IMAGE=hexpm/elixir:1.19.5-erlang-28.4.3-debian-bookworm-20260824-slim
FROM ${ELIXIR_IMAGE} AS build

# git: the CLI embeds the commit sha at compile time.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
ENV MIX_ENV=prod

RUN mix local.hex --force \
    && mix local.rebar --force

# Fetch deps in their own layer so source edits do not refetch them.
COPY mix.exs mix.lock ./
COPY apps/arc_cli/mix.exs apps/arc_cli/mix.exs
COPY apps/arc_control/mix.exs apps/arc_control/mix.exs
COPY apps/arc_data/mix.exs apps/arc_data/mix.exs
COPY apps/arc_identity/mix.exs apps/arc_identity/mix.exs
COPY apps/arc_mcp/mix.exs apps/arc_mcp/mix.exs
COPY apps/arc_net/mix.exs apps/arc_net/mix.exs
COPY apps/arc_storage/mix.exs apps/arc_storage/mix.exs
RUN mix deps.get --only prod \
    && mix deps.compile

COPY config config
COPY apps apps
COPY rel rel
COPY .git .git
RUN mix release arc_runtime --overwrite

# Runtime stage: only what the release needs to run.
FROM debian:bookworm-slim AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates libncurses6 libstdc++6 locales openssl \
    && sed -i '/en_US.UTF-8/s/^# //' /etc/locale.gen \
    && locale-gen \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 arc \
    && mkdir -p /home/arc/.config/arc /home/arc/.arc \
    && chown -R arc:arc /home/arc

ENV LANG=en_US.UTF-8 \
    LC_ALL=en_US.UTF-8 \
    HOME=/home/arc \
    PATH=/app/bin:$PATH

COPY --from=build --chown=arc:arc /src/_build/prod/rel/arc_runtime /app

USER arc
WORKDIR /home/arc

# Keys, control state, and caches live under $HOME/.config/arc and
# $HOME/.arc. Mount a volume there to keep the relay identity across runs.
VOLUME ["/home/arc/.config/arc"]

EXPOSE 7331
ENTRYPOINT ["arc"]
CMD ["relay"]
