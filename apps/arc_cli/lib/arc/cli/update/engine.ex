defmodule Arc.CLI.Update.Engine do
  @moduledoc false

  alias Arc.CLI.Update.{Manifest, Observer, Package, Source, Store}
  alias Arc.Data.Agent

  def run(operation, config) when operation in ["check", "apply"] do
    with {:ok, installed} <- running(),
         {:ok, source, cleanup} <- open_source(config.update.source) do
      try do
        with {:ok, verified, selection} <- check(source, installed, config.update) do
          execute(operation, source, installed, verified, selection, config.update)
        end
      after
        cleanup.()
      end
    end
  rescue
    _ -> {:error, :update_failed}
  catch
    :exit, _ -> {:error, :update_failed}
  end

  def running_document do
    case running() do
      {:ok, installed} -> Map.take(installed, ~w(version build runtime platform))
      {:error, reason} -> %{"state" => "unavailable", "reason" => Atom.to_string(reason)}
    end
  end

  defp running do
    with root when is_binary(root) <- System.get_env("RELEASE_ROOT"),
         true <- Path.type(root) == :absolute,
         releases when is_list(releases) <- :release_handler.which_releases(),
         {_name, version, _, _} <-
           Enum.find(releases, &(elem(&1, 3) == :current)) ||
             Enum.find(releases, &(elem(&1, 3) == :permanent)),
         version <- List.to_string(version),
         true <- Regex.match?(~r/\A[0-9A-Za-z.+_-]+\z/, version),
         path <- Path.join([root, "releases", version, "arc-build.json"]),
         {:ok, %{type: :regular, size: size}} when size <= 16_384 <- File.lstat(path),
         {:ok, bytes} <- File.read(path),
         %{"version" => ^version, "build" => build, "runtime" => runtime, "platform" => platform} =
           document <- :json.decode(bytes),
         true <- is_binary(build) and build != "",
         true <- runtime == to_string(:erlang.system_info(:version)),
         true <- platform == platform() do
      {:ok, Map.put(document, "root", root)}
    else
      _ -> {:error, :upgrade_capable_base_required}
    end
  rescue
    _ -> {:error, :upgrade_capable_base_required}
  catch
    :exit, _ -> {:error, :upgrade_capable_base_required}
  end

  def platform do
    os = :os.type() |> elem(1) |> Atom.to_string()
    arch = :erlang.system_info(:system_architecture) |> to_string() |> String.split("-") |> hd()
    %{"os" => os, "arch" => arch}
  end

  defp check(source, installed, config) do
    with {:ok, checkpoint} <- Store.read(config.state_dir, "checkpoint"),
         :ok <- check_publisher(checkpoint, config.publisher),
         previous <- get_in(checkpoint, ["channels", config.channel]) || %{},
         {:ok, bytes} <- Source.channel(source, config.channel),
         {:ok, document} <- decode(bytes),
         {:ok, verified} <-
           Manifest.verify(document,
             expected_publisher: config.publisher,
             expected_channel: config.channel,
             last_sequence: previous["sequence"],
             last_digest: previous["digest"],
             now: System.system_time(:second)
           ),
         :ok <- save_checkpoint(config, checkpoint, verified),
         {:ok, selection} <-
           Manifest.select(verified,
             installed_build: installed["build"],
             installed_version: installed["version"],
             installed_runtime: installed["runtime"],
             platform: installed["platform"],
             pin: config.pin
           ) do
      {:ok, verified, selection}
    end
  end

  defp check_publisher(%{"publisher" => publisher}, publisher), do: :ok
  defp check_publisher(checkpoint, _) when map_size(checkpoint) == 0, do: :ok
  defp check_publisher(_, _), do: {:error, :publisher_change_requires_trust_transition}

  defp save_checkpoint(config, previous, verified) do
    channels = Map.get(previous, "channels", %{})

    record = %{"sequence" => verified.manifest["sequence"], "digest" => verified.digest}

    Store.write(config.state_dir, "checkpoint", %{
      "publisher" => config.publisher,
      "channels" => Map.put(channels, config.channel, record)
    })
  end

  defp execute("check", _, _, _, selection, _), do: {:ok, selection_document(selection)}
  defp execute("apply", _, _, _, %{eligible: nil}, _), do: {:error, :no_eligible_hot_update}

  defp execute("apply", source, installed, verified, selection, config) do
    %{release: release, source: edge} = selection.eligible
    root = installed["root"]
    archive = Path.join([root, "releases", "arc_runtime.tar.gz"])

    with :ok <- new_release(root, release["version"]),
         true <- release["runtime"] == installed["runtime"] or {:error, :runtime_change},
         {:ok, ^archive} <-
           Source.stage(source, release["sha256"], release["size"], archive),
         {:ok, package} <-
           Package.validate_archive(archive,
             expected: release,
             source_version: installed["version"],
             installed_root: root
           ),
         true <-
           (package.relup_sha256 == edge["upgrade_plan_sha256"] and
              package.relup_sha256 == edge["downgrade_plan_sha256"]) or
             {:error, :plan_digest_mismatch},
         :ok <- destinations_absent(root, package.entries),
         true <-
           verified.manifest["expires_at"] > System.system_time(:second) or
             {:error, :expired_manifest},
         :ok <- journal(config, "applying", installed, release),
         {:ok, version} <- :release_handler.unpack_release(~c"arc_runtime"),
         true <- to_string(version) == release["version"] or {:error, :release_version_mismatch},
         {:ok, _, _} <- :release_handler.check_install_release(version),
         {:ok, observer} <- Observer.start(Process.whereis(Arc.Net.Relay)) do
      install(version, observer, installed, release, config)
    else
      {:error, reason} -> {:error, reason}
      _ -> {:error, :release_preflight_failed}
    end
  end

  defp install(version, observer, installed, release, config) do
    with {:ok, _, _} <-
           :release_handler.install_release(version,
             code_change_timeout: 5_000,
             suspend_timeout: 5_000
           ),
         :ok <- journal(config, "observing", installed, release),
         :ok <- observe(),
         {:ok, observation} <- Observer.finish(observer),
         :ok <- journal(config, "committing", installed, release),
         :ok <- :release_handler.make_permanent(version) do
      {:ok,
       %{
         "state" => "current",
         "version" => release["version"],
         "build" => release["build"],
         "observation" => observation
       }}
    else
      _ -> {:error, :installation_requires_reconciliation}
    end
  after
    Observer.abort(observer)
  end

  defp observe do
    Process.sleep(1_000)
    :ok
  end

  defp journal(config, phase, installed, release) do
    Store.write(config.state_dir, "journal", %{
      "state" => phase,
      "from" => installed["build"],
      "to" => release["build"],
      "target_version" => release["version"],
      "root" => installed["root"],
      "archive_sha256" => release["sha256"],
      "recorded_at" => System.system_time(:second)
    })
  end

  defp new_release(root, version) do
    if Regex.match?(~r/\A[0-9A-Za-z.+_-]+\z/, version) and
         match?({:error, :enoent}, File.lstat(Path.join([root, "releases", version]))) do
      :ok
    else
      {:error, :release_destination_exists}
    end
  end

  # The package builder must omit unchanged libraries. In particular, unpacking
  # must never overwrite a file from the running release, even with equal bytes.
  defp destinations_absent(root, entries) do
    Enum.reduce_while(entries, :ok, fn entry, :ok ->
      case File.lstat(Path.join(root, entry)) do
        {:error, :enoent} -> {:cont, :ok}
        {:ok, %{type: :directory}} -> {:cont, :ok}
        _ -> {:halt, {:error, :release_destination_exists}}
      end
    end)
  end

  defp selection_document(selection) do
    %{
      "state" => Atom.to_string(selection.status),
      "reason" => Atom.to_string(selection.reason),
      "latest" => brief(selection.latest),
      "eligible" => if(selection.eligible, do: brief(selection.eligible.release), else: :null)
    }
  end

  defp brief(nil), do: :null
  defp brief(release), do: Map.take(release, ~w(version build restart_required))

  defp decode(bytes) do
    {:ok, :json.decode(bytes)}
  rescue
    _ -> {:error, :invalid_channel_document}
  end

  defp open_source(%{local_dir: root}), do: {:ok, {:local, root}, fn -> :ok end}

  defp open_source(%{uri: uri, relay: address, relay_pubkey: pin}) do
    identity = Arc.Identity.generate()
    {host, port} = Arc.Net.relay_address_from(address)
    key = Arc.Net.relay_pubkey_from(pin)

    case Agent.start_link(identity) do
      {:ok, agent} ->
        cleanup = fn ->
          Arc.Net.release_relay(identity.public_key, agent)
          if Process.alive?(agent), do: GenServer.stop(agent, :normal)
        end

        case initialize_agent(agent, identity, host, port, key) do
          :ok ->
            {:ok, {:arc, agent, uri}, cleanup}

          error ->
            cleanup.()
            error
        end

      _ ->
        {:error, :update_transport_unavailable}
    end
  end

  defp initialize_agent(agent, identity, host, port, key) do
    with :ok <- Arc.Net.acquire_relay(host, port, identity, key, owner_pid: agent),
         do: Agent.publish_relay(agent)
  end
end
