defmodule Arc.CLI.Update.SelfUpdate do
  @moduledoc """
  Updates this ARC installation from a release provider reached only through
  the configured relay.

  `arc update` connects to the joined relay as the selected citizen identity,
  finds a `releases` provider there (or uses an explicit `--source`), fetches
  the selected channel document, verifies it
  against the trusted release publisher with replay protection, and compares
  the newest eligible complete archive with the installed release. `apply`
  downloads that archive through the relay, validates it, and hands it to
  `Arc.CLI.Update.Installer`. `check` stops after the comparison. `status`
  reads local state only.

  Trust is a local decision: the publisher key given with `--publisher` is
  remembered under the update state directory once a channel it signed has
  verified, and a different key is refused until `--replace-publisher` is
  passed explicitly. Relay and provider keys identify transport participants
  only; they never authorize a release.
  """

  alias Arc.CLI.RelaySettings
  alias Arc.CLI.Update.{Engine, Installer, Manifest, Source, Store}
  alias Arc.Data.{Agent, CapabilityDiscovery, Protocol}
  alias Arc.Identity
  alias Arc.Identity.KeyStore

  @channels ["stable", "beta"]
  @max_install_bytes 1_024 * 1_024 * 1_024
  @stage_deadline_ms 1_800_000
  @max_candidates 5
  @discovery_limit 50
  @hex ~r/\A[0-9a-fA-F]{64}\z/

  @spec run(String.t(), keyword()) :: {:ok, map()} | {:error, term()}
  def run(operation, opts) when operation in ["status", "check", "apply"] and is_list(opts) do
    with {:ok, state_dir} <- state_dir(),
         {:ok, settings} <- Store.read(state_dir, "settings"),
         {:ok, channel} <- resolve_channel(opts, settings),
         {:ok, installed} <- installed_release(),
         {:ok, journal} <- Store.read(state_dir, "journal") do
      context = %{
        opts: opts,
        state_dir: state_dir,
        settings: settings,
        channel: channel,
        installed: installed,
        journal: journal
      }

      execute(operation, context)
    end
  end

  def run(_operation, _opts), do: {:error, :invalid_update_request}

  @doc "The directory holding the installation's update settings, checkpoint, and journal."
  @spec state_dir() :: {:ok, String.t()} | {:error, term()}
  def state_dir do
    root =
      Application.get_env(:arc_cli, :update_state_dir) ||
        Path.join([System.user_home!(), ".config", "arc", "update"])

    with :ok <- File.mkdir_p(root),
         :ok <- File.chmod(root, 0o700) do
      {:ok, root}
    else
      _ -> {:error, :invalid_update_state}
    end
  end

  defp execute("status", context) do
    with {:ok, checkpoint} <- Store.read(context.state_dir, "checkpoint") do
      publisher = context.settings["publisher"]
      previous = get_in(checkpoint, ["channels", context.channel]) || %{}

      {:ok,
       context
       |> base_document(publisher)
       |> Map.merge(%{
         "state" => Map.get(context.journal, "state", "idle"),
         "reason" => Map.get(context.journal, "reason", :null),
         "last_sequence" => Map.get(previous, "sequence", :null),
         "state_dir" => context.state_dir,
         "message" => status_message(context, publisher)
       })}
    end
  end

  defp execute(operation, context) do
    with {:ok, publisher} <- resolve_publisher(context.opts, context.settings),
         :ok <- ensure_not_interrupted(context),
         {:ok, relay} <- relay_settings(context.opts),
         {:ok, identity} <- active_identity(),
         {:ok, checkpoint} <- read_checkpoint(context, publisher),
         {:ok, document} <-
           with_relay(identity, relay, fn agent ->
             evaluate(operation, context, publisher, checkpoint, agent)
           end) do
      {:ok,
       document
       |> Map.put("relay", relay_label(relay))
       |> Map.put("identity", Identity.name(identity))}
    end
  end

  defp evaluate(operation, context, publisher, checkpoint, agent) do
    with {:ok, candidates} <- candidates(context.opts, agent),
         {:ok, verified, candidate} <-
           first_verified(candidates, agent, context.channel, publisher, checkpoint),
         :ok <- save_checkpoint(context, publisher, checkpoint, verified),
         :ok <- remember_settings(context, publisher),
         {:ok, selection} <-
           Manifest.select_install(verified,
             installed_version: context.installed.version,
             platform: context.installed.platform
           ) do
      document = selection_document(context, publisher, candidate, selection)

      if operation == "apply" and selection.status == :available do
        apply_install(context, agent, candidate, selection.install, document)
      else
        {:ok, document}
      end
    end
  end

  defp apply_install(context, agent, candidate, release, document) do
    %{"sha256" => sha256, "size" => size} = release["install"]
    archive = Path.join([context.state_dir, "staging", sha256 <> ".tar.gz"])
    source = {:arc, agent, candidate.uri}

    result =
      with :ok <- prepare_staging(archive),
           :ok <- journal(context, "staging", release, nil),
           {:ok, ^archive} <-
             Source.stage(source, sha256, size, archive,
               capability: candidate.capability,
               max_bytes: @max_install_bytes,
               deadline_ms: @stage_deadline_ms
             ),
           {:ok, _} <-
             Installer.validate_archive(archive,
               version: release["version"],
               sha256: sha256,
               size: size
             ),
           :ok <- journal(context, "installing", release, nil),
           {:ok, installed} <-
             Installer.install(archive, context.installed.root, version: release["version"]),
           :ok <- journal(context, "current", release, nil) do
        {:ok, installed}
      end

    _ = File.rm(archive)

    case result do
      {:ok, installed} ->
        {:ok,
         Map.merge(document, %{
           "state" => "current",
           "reason" => "installed",
           "previous" => installed.previous,
           "message" =>
             "Installed arc #{installed.version} at #{installed.root}. The replaced release " <>
               "is kept at #{installed.previous} until the next update."
         })}

      {:error, reason} ->
        _ = journal(context, "failed", release, reason_code(reason))
        {:error, reason}
    end
  end

  defp prepare_staging(archive) do
    directory = Path.dirname(archive)

    with :ok <- File.mkdir_p(directory),
         :ok <- File.chmod(directory, 0o700),
         :ok <- remove_stale_archive(archive) do
      :ok
    else
      _ -> {:error, :staging_directory_failed}
    end
  end

  defp remove_stale_archive(archive) do
    case File.lstat(archive) do
      {:error, :enoent} -> :ok
      {:ok, %{type: :regular}} -> File.rm(archive)
      _ -> {:error, :staging_directory_failed}
    end
  end

  defp journal(context, phase, release, reason) do
    document = %{
      "state" => phase,
      "target_version" => release["version"],
      "target_build" => release["build"],
      "root" => context.installed.root,
      "archive_sha256" => release["install"]["sha256"],
      "recorded_at" => System.system_time(:second)
    }

    document = if reason, do: Map.put(document, "reason", reason), else: document

    case Store.write(context.state_dir, "journal", document) do
      :ok -> :ok
      _ -> {:error, :update_state_write_failed}
    end
  end

  defp ensure_not_interrupted(%{journal: %{"state" => "installing"} = journal}),
    do: {:error, {:interrupted_update, journal["root"]}}

  defp ensure_not_interrupted(_context), do: :ok

  defp resolve_channel(opts, settings) do
    channel = Keyword.get(opts, :channel) || settings["channel"] || "stable"
    if channel in @channels, do: {:ok, channel}, else: {:error, :invalid_channel}
  end

  defp resolve_publisher(opts, settings) do
    stored = settings["publisher"]
    replace? = Keyword.get(opts, :replace_publisher, false)

    case normalize_publisher(Keyword.get(opts, :publisher)) do
      {:ok, nil} when replace? ->
        {:error, :replace_publisher_requires_publisher}

      {:ok, nil} ->
        default_publisher(stored)

      {:ok, publisher} when is_binary(stored) and stored != publisher and not replace? ->
        {:error, :publisher_change_requires_trust_transition}

      {:ok, publisher} ->
        {:ok, publisher}

      {:error, _reason} = error ->
        error
    end
  end

  defp default_publisher(stored) when is_binary(stored), do: {:ok, stored}

  defp default_publisher(nil) do
    case normalize_publisher(Application.get_env(:arc_cli, :release_publisher)) do
      {:ok, nil} -> {:error, :publisher_required}
      {:ok, publisher} -> {:ok, publisher}
      {:error, _} -> {:error, :publisher_required}
    end
  end

  defp normalize_publisher(nil), do: {:ok, nil}

  defp normalize_publisher(value) when is_binary(value) do
    if Regex.match?(@hex, value),
      do: {:ok, String.downcase(value)},
      else: {:error, :invalid_publisher}
  end

  defp normalize_publisher(_), do: {:error, :invalid_publisher}

  # The settings file records explicit trust and channel decisions only, and
  # only after the publisher's signature has actually verified once.
  defp remember_settings(context, publisher) do
    document = %{"publisher" => publisher, "channel" => context.channel}

    if Map.take(context.settings, ["publisher", "channel"]) == document do
      :ok
    else
      case Store.write(context.state_dir, "settings", document) do
        :ok -> :ok
        _ -> {:error, :update_state_write_failed}
      end
    end
  end

  # The checkpoint records the highest accepted sequence per channel for one
  # publisher. Replacing the publisher starts a fresh checkpoint.
  defp read_checkpoint(context, publisher) do
    with {:ok, checkpoint} <- Store.read(context.state_dir, "checkpoint") do
      cond do
        map_size(checkpoint) == 0 -> {:ok, checkpoint}
        checkpoint["publisher"] == publisher -> {:ok, checkpoint}
        Keyword.get(context.opts, :replace_publisher, false) -> {:ok, %{}}
        true -> {:error, :publisher_change_requires_trust_transition}
      end
    end
  end

  defp save_checkpoint(context, publisher, checkpoint, verified) do
    channels = Map.get(checkpoint, "channels", %{})
    record = %{"sequence" => verified.manifest["sequence"], "digest" => verified.digest}

    case Store.write(context.state_dir, "checkpoint", %{
           "publisher" => publisher,
           "channels" => Map.put(channels, context.channel, record)
         }) do
      :ok -> :ok
      _ -> {:error, :update_state_write_failed}
    end
  end

  defp relay_settings(opts) do
    case RelaySettings.resolve(Keyword.take(opts, [:relay, :relay_pubkey])) do
      {:ok, %{relay: {host, port}, relay_pubkey: pin}} when is_binary(pin) ->
        {:ok, {host, port, pin}}

      {:ok, %{relay: nil}} ->
        {:error, :relay_required}

      {:ok, _} ->
        {:error, :relay_pin_required}

      {:error, :invalid_relay_config} ->
        {:error, :invalid_relay_configuration}
    end
  end

  defp relay_label({host, port, _pin}), do: "#{host}:#{port}"

  # The update session runs as the selected citizen, exactly like discover
  # and request: the same key `arc join` created or `arc keys use` chose.
  defp active_identity do
    case KeyStore.resolve_active() do
      {:ok, identity} -> {:ok, identity}
      {:error, reason} -> {:error, {:identity, reason}}
    end
  end

  defp with_relay(identity, {host, port, pin}, fun) do
    case Agent.start_link(identity) do
      {:ok, agent} ->
        try do
          with :ok <- connect(agent, identity, host, port, pin) do
            fun.(agent)
          end
        after
          if Process.alive?(agent), do: GenServer.stop(agent, :normal)
        end

      _ ->
        {:error, :update_transport_unavailable}
    end
  end

  defp connect(agent, identity, host, port, pin) do
    with :ok <- Arc.Net.connect_relay(host, port, identity, pin),
         :ok <- Agent.publish_relay(agent) do
      :ok
    else
      {:error, reason} -> {:error, {:relay_connect_failed, reason}}
      _ -> {:error, {:relay_connect_failed, :unavailable}}
    end
  end

  defp candidates(opts, agent) do
    case Keyword.get(opts, :source) do
      nil -> discover(agent)
      uri -> explicit_source(uri)
    end
  end

  defp explicit_source(uri) do
    case Protocol.parse(uri) do
      {:ok, %{scheme: "releases", provider: provider}} ->
        {:ok, [%{uri: uri, capability: "primary", provider: provider}]}

      _ ->
        {:error, :invalid_source}
    end
  end

  defp discover(agent) do
    case CapabilityDiscovery.discover(agent, "releases", limit: @discovery_limit) do
      {:ok, %{matches: matches}} ->
        matches
        |> Enum.filter(&(&1.capability["scheme"] == "releases"))
        |> Enum.map(&candidate/1)
        |> Enum.uniq_by(& &1.uri)
        |> Enum.take(@max_candidates)
        |> case do
          [] -> {:error, :no_release_provider}
          candidates -> {:ok, candidates}
        end

      {:error, reason} ->
        {:error, {:discovery_failed, reason}}
    end
  end

  defp candidate(%{provider: provider, capability: capability}) do
    key = provider["public_key"]

    %{
      uri: "releases+arc://#{key}/releases",
      capability: capability["id"] || "primary",
      provider: key
    }
  end

  # Any provider may host the channel; the publisher signature decides trust.
  # Candidates are tried in discovery order until one document verifies.
  defp first_verified(candidates, agent, channel, publisher, checkpoint) do
    previous = get_in(checkpoint, ["channels", channel]) || %{}

    Enum.reduce_while(candidates, {:error, :no_release_provider}, fn candidate, _acc ->
      case fetch_verified(candidate, agent, channel, publisher, previous) do
        {:ok, verified} -> {:halt, {:ok, verified, candidate}}
        {:error, reason} -> {:cont, {:error, {:channel_rejected, candidate.provider, reason}}}
      end
    end)
  end

  defp fetch_verified(candidate, agent, channel, publisher, previous) do
    with {:ok, bytes} <-
           Source.channel({:arc, agent, candidate.uri}, channel, capability: candidate.capability),
         {:ok, document} <- decode(bytes) do
      Manifest.verify(document,
        expected_publisher: publisher,
        expected_channel: channel,
        last_sequence: previous["sequence"],
        last_digest: previous["digest"],
        now: System.system_time(:second)
      )
    end
  end

  defp decode(bytes) do
    case :json.decode(bytes) do
      document when is_map(document) -> {:ok, document}
      _ -> {:error, :invalid_channel_document}
    end
  rescue
    _ -> {:error, :invalid_channel_document}
  end

  defp installed_release do
    with root when is_binary(root) <- System.get_env("RELEASE_ROOT") || :missing,
         :ok <- Installer.verify_root(root),
         {:ok, version} <- Installer.installed_version(root) do
      {:ok,
       %{
         root: root,
         version: version,
         running: running_version(),
         platform: Engine.platform()
       }}
    else
      :missing -> {:error, :not_installed_release}
      {:error, :install_root_not_absolute} -> {:error, :not_installed_release}
      {:error, _reason} = error -> error
    end
  end

  defp running_version do
    case Application.spec(:arc_cli, :vsn) do
      vsn when is_list(vsn) -> List.to_string(vsn)
      _ -> :null
    end
  end

  defp base_document(context, publisher) do
    %{
      "target" => "installation",
      "channel" => context.channel,
      "publisher" => publisher || :null,
      "installed" => %{
        "version" => context.installed.version,
        "running" => context.installed.running,
        "root" => context.installed.root,
        "platform" => context.installed.platform
      }
    }
  end

  defp selection_document(context, publisher, candidate, selection) do
    context
    |> base_document(publisher)
    |> Map.merge(%{
      "state" => Atom.to_string(selection.status),
      "reason" => Atom.to_string(selection.reason),
      "latest" => brief(selection.latest),
      "install" => brief_install(selection.install),
      "source" => candidate.uri,
      "message" => selection_message(context, selection)
    })
  end

  defp brief(nil), do: :null
  defp brief(release), do: Map.take(release, ~w(version build))

  defp brief_install(nil), do: :null

  defp brief_install(release),
    do: release |> Map.take(~w(version build)) |> Map.put("size", release["install"]["size"])

  defp selection_message(context, %{status: :available, install: install}) do
    "arc #{install["version"]} is available for #{context.installed.version}; " <>
      "run arc update to install it."
  end

  defp selection_message(context, %{status: :up_to_date}),
    do: "arc #{context.installed.version} is up to date on the #{context.channel} channel."

  defp selection_message(context, %{reason: :no_platform_release}) do
    "the #{context.channel} channel has no release for " <>
      "#{context.installed.platform["os"]}/#{context.installed.platform["arch"]}."
  end

  defp selection_message(_context, %{reason: :lower_semver_target}) do
    "the channel's newest release is older than this installation; " <>
      "no downgrade is performed."
  end

  defp selection_message(_context, %{reason: :no_install_archive, latest: latest}) do
    "release #{latest["version"]} is published without a complete archive " <>
      "for this platform; nothing was installed."
  end

  defp selection_message(_context, selection), do: Atom.to_string(selection.reason)

  defp status_message(_context, nil),
    do: "no release publisher is trusted yet; run arc update --publisher KEY."

  defp status_message(context, _publisher),
    do: "arc #{context.installed.version} installed at #{context.installed.root}."

  @doc "A one-line operator message for an error returned by `run/2`."
  @spec describe_error(term()) :: String.t()
  def describe_error(:not_installed_release),
    do:
      "arc update replaces a release installation, and this arc is not running from one " <>
        "(RELEASE_ROOT is unset); reinstall with install.sh instead"

  def describe_error(:install_root_invalid),
    do: "the installation root under RELEASE_ROOT is missing bin/arc or releases/start_erl.data"

  def describe_error(:installed_version_unreadable),
    do: "the installed release version could not be read from releases/start_erl.data"

  def describe_error(:publisher_required),
    do: "no release publisher is trusted yet; run arc update --publisher PUBLISHER_KEY once"

  def describe_error(:invalid_publisher),
    do: "--publisher must be the release publisher's 64-character hex public key"

  def describe_error(:replace_publisher_requires_publisher),
    do: "--replace-publisher needs --publisher KEY"

  def describe_error(:publisher_change_requires_trust_transition),
    do:
      "the release publisher differs from the trusted key; pass --publisher KEY " <>
        "--replace-publisher to trust the new publisher explicitly"

  def describe_error(:invalid_channel), do: "--channel must be stable or beta"

  def describe_error(:invalid_source),
    do: "--source must be releases+arc://<provider public key>/releases"

  def describe_error(:relay_required),
    do: "join a relay first (arc join HOST[:PORT]) or pass --relay and --relay-pubkey"

  def describe_error(:relay_pin_required),
    do: "set --relay-pubkey or ARC_RELAY_PUBKEY to the relay's public key"

  def describe_error(:invalid_relay_configuration),
    do: "relay settings are invalid; run arc join again or provide --relay and --relay-pubkey"

  def describe_error({:identity, reason}), do: Arc.CLI.Keys.describe_error(reason)

  def describe_error({:relay_connect_failed, reason}),
    do: "relay connection failed: " <> reason_code(reason)

  def describe_error(:update_transport_unavailable), do: "the update session could not start"

  def describe_error(:no_release_provider),
    do: "no release provider is published on this relay; pass --source to name one"

  def describe_error({:discovery_failed, reason}),
    do: "relay discovery failed: " <> reason_code(reason)

  def describe_error({:channel_rejected, provider, reason}),
    do:
      "channel document from provider #{String.slice(provider, 0, 12)}… was rejected: " <>
        reason_code(reason)

  def describe_error({:interrupted_update, root}),
    do:
      "a previous update was interrupted while replacing #{root}; verify #{root} and " <>
        "#{root}.previous, then delete journal.json under the update state directory"

  def describe_error(:invalid_update_state),
    do: "the update state directory is unreadable or unwritable"

  def describe_error(:update_state_write_failed),
    do: "the update state directory could not be written"

  def describe_error(:staging_directory_failed),
    do: "the download staging directory could not be prepared"

  def describe_error({:install_unrecoverable, root, _reason, _restore}),
    do:
      "installation failed after moving #{root} aside; restore it from #{root}.previous " <>
        "or reinstall with install.sh"

  def describe_error({:candidate_start_failed, reason}),
    do: "the downloaded release did not start: " <> reason_code(reason)

  def describe_error({:candidate_reports_other_version, line}),
    do: "the downloaded release reported an unexpected version: " <> line

  def describe_error(:invalid_update_request), do: "unsupported update request"
  def describe_error(reason), do: "update failed: " <> reason_code(reason)

  defp reason_code(reason) when is_atom(reason), do: Atom.to_string(reason)

  defp reason_code(reason) when is_binary(reason) do
    if Regex.match?(~r/\A[A-Za-z0-9_ .:-]{1,120}\z/, reason), do: reason, else: "update_failed"
  end

  defp reason_code(reason) when is_tuple(reason) and tuple_size(reason) > 0 do
    case Tuple.to_list(reason) do
      [code, detail | _] when is_atom(code) and (is_atom(detail) or is_binary(detail)) ->
        Atom.to_string(code) <> " (" <> reason_code(detail) <> ")"

      [code | _] when is_atom(code) ->
        Atom.to_string(code)

      _ ->
        "update_failed"
    end
  end

  defp reason_code(_), do: "update_failed"
end
