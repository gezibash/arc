# A relay and the DM provider in one image, for one Fly.io machine.
#
# This file builds everything from this checkout, so a remote builder needs no
# other image:
#   fly deploy -c docker/fly/fly.toml --dockerfile docker/fly/Dockerfile
ARG ELIXIR_IMAGE=hexpm/elixir:1.19.5-erlang-28.4.3-debian-bookworm-20260824-slim

# Stage 1: the ARC release with its bundled Erlang runtime.
FROM ${ELIXIR_IMAGE} AS build

# git: the CLI embeds the commit sha at compile time.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
ENV MIX_ENV=prod

RUN mix local.hex --force \
    && mix local.rebar --force

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

# Stage 2: the DM provider escript.
FROM ${ELIXIR_IMAGE} AS providers

ENV MIX_ENV=prod
RUN mix local.hex --force
# Mix releases omit this boot file, which escript needs.
RUN elixir -e 'File.cp!(Path.join(to_string(:code.root_dir()), "bin/no_dot_erlang.boot"), "/tmp/no_dot_erlang.boot")'

WORKDIR /src/dm
COPY providers/dm/mix.exs ./
COPY providers/dm/lib ./lib
RUN mix escript.build

# Stage 3: the machine image.
FROM debian:bookworm-slim

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
COPY --from=providers /tmp/no_dot_erlang.boot /app/bin/no_dot_erlang.boot
COPY --from=providers --chown=arc:arc /src/dm/dm /opt/arc-providers/dm/dm
COPY --chown=arc:arc providers/dm/Arcfile providers/dm/manifest.json /opt/arc-providers/dm/
COPY --chmod=755 docker/local/run-provider.sh /opt/arc-providers/dm/run.sh
COPY --chmod=755 docker/fly/entrypoint.sh /opt/arc-fly/entrypoint.sh

USER arc
WORKDIR /home/arc
VOLUME ["/home/arc/.config/arc"]
EXPOSE 7331

ENTRYPOINT ["/opt/arc-fly/entrypoint.sh"]
