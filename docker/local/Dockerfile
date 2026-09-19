# Keep the released core image unchanged; package providers alongside it.
ARG ARC_IMAGE=ghcr.io/gezibash/arc:0.5.1
FROM hexpm/elixir:1.19.5-erlang-28.4.3-debian-bookworm-20260824-slim AS providers

ENV MIX_ENV=prod
RUN mix local.hex --force
# Mix releases omit this boot file, which escript needs. The builder uses the
# same OTP version as the pinned ARC image, including kernel/stdlib versions.
RUN elixir -e 'File.cp!(Path.join(to_string(:code.root_dir()), "bin/no_dot_erlang.boot"), "/tmp/no_dot_erlang.boot")'

WORKDIR /src/journal
COPY providers/journal/mix.exs providers/journal/mix.lock ./
RUN mix deps.get --only prod
COPY providers/journal/lib ./lib
RUN mix escript.build

WORKDIR /src/dm
COPY providers/dm/mix.exs ./
COPY providers/dm/lib ./lib
RUN mix escript.build

WORKDIR /src/agora
COPY providers/agora/mix.exs ./
COPY providers/agora/lib ./lib
RUN mix escript.build

WORKDIR /src/releases
COPY providers/releases/mix.exs ./
COPY providers/releases/lib ./lib
RUN mix escript.build

FROM ${ARC_IMAGE}
USER root

# Journal uses Git; Agora uses ps to check stale writer locks after a crash.
# Provider escripts use ARC's bundled OTP.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git procps \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /home/arc/public /home/arc/.arc/journal /home/arc/.arc/dm \
        /home/arc/.arc/agora /home/arc/releases /run/arc/relay /run/arc/journal /run/arc/dm /run/arc/agora /run/arc/releases \
    && chown -R arc:arc /home/arc/public /home/arc/.arc /run/arc

COPY --from=providers /src/journal/journal /opt/arc-providers/journal/journal
COPY --from=providers /src/dm/dm /opt/arc-providers/dm/dm
COPY --from=providers /src/agora/agora /opt/arc-providers/agora/agora
COPY --from=providers /src/releases/releases /opt/arc-providers/releases/releases
COPY --from=providers /tmp/no_dot_erlang.boot /app/bin/no_dot_erlang.boot
COPY providers/journal/Arcfile providers/journal/manifest.json /opt/arc-providers/journal/
COPY providers/dm/Arcfile providers/dm/manifest.json /opt/arc-providers/dm/
COPY providers/agora/Arcfile providers/agora/manifest.json /opt/arc-providers/agora/
COPY providers/releases/Arcfile providers/releases/manifest.json /opt/arc-providers/releases/
COPY --chmod=755 docker/local/run-provider.sh /opt/arc-providers/journal/run.sh
COPY --chmod=755 docker/local/run-provider.sh /opt/arc-providers/dm/run.sh
COPY --chmod=755 docker/local/run-provider.sh /opt/arc-providers/agora/run.sh
COPY --chmod=755 docker/local/run-provider.sh /opt/arc-providers/releases/run.sh
COPY --chmod=755 docker/local/entrypoint.sh docker/local/relay-health.sh /opt/arc-local/

USER arc
ENTRYPOINT ["/opt/arc-local/entrypoint.sh"]
CMD ["relay"]
