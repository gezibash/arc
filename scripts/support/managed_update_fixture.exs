#!/usr/bin/env elixir

# Full, disposable proof for the managed native relay update path. It builds a
# fresh release beneath one private temporary root, gives that release a
# synthetic Relay-only edge, then drives the public local operator commands.
# It never opens an installed ARC root or a user key store.

defmodule Arc.ManagedUpdate.Proof do
  alias Arc.CLI.Update.Manifest
  alias Arc.Data.Agent
  alias Arc.Identity

  @release "arc_runtime"
  @base_version Mix.Project.config()[:version]
  @target_version @base_version
                  |> Version.parse!()
                  |> Map.update!(:patch, &(&1 + 1))
                  |> to_string()
  @base_build "managed-proof-base"
  @target_build "managed-proof-target"
  @release_provider Path.expand("../../providers/releases", __DIR__)

  def run do
    root = make_root()

    try do
      paths = setup(root)
      build_base(paths)
      prepare_base(paths)
      build_candidate(paths)
      build_package(paths)
      paths = publish_channel(paths)
      run_service_update(paths)
      IO.puts("managed update proof: PASS")
    after
      stop_service(Process.get(:managed_update_proof_port))
      remove_root(root)
    end
  end

  @doc """
  Prove a managed relay can fetch and apply an update from a network-scoped
  release provider across one approved federation edge.
  """
  def run_federation do
    root = make_root()

    try do
      paths = setup(root)
      build_base(paths)
      prepare_base(paths)
      build_candidate(paths)
      build_package(paths)
      paths = publish_federated_channel(paths)
      remote = start_federated_provider(paths)

      try do
        run_service_update(paths, federation: remote)
        IO.puts("managed federation update proof: PASS")
      after
        stop_federated_provider(remote)
      end
    after
      stop_service(Process.get(:managed_update_proof_port))
      remove_root(root)
    end
  end

  def setup(root) do
    Enum.each(~w(provider channels blobs state), &File.mkdir_p!(Path.join(root, &1)))

    %{
      root: root,
      base: Path.join(root, "base"),
      candidate: Path.join(root, "candidate"),
      package: Path.join(root, "candidate.tar.gz"),
      provider: Path.join(root, "provider"),
      state: Path.join(root, "state"),
      config: Path.join(root, "service.json")
    }
  end

  def build_base(paths) do
    command!("mix", ["release", @release, "--path", paths.base], env: [{"MIX_ENV", "prod"}])
  end

  def prepare_base(paths) do
    isolate_runtime(paths)
    compile_relay(paths.base, :base)

    command!(
      "mix",
      [
        "run",
        "--no-start",
        "scripts/prepare-hot-base.exs",
        "--root",
        paths.base,
        "--build",
        @base_build,
        "--os",
        platform_os(),
        "--arch",
        platform_arch()
      ],
      env: [{"MIX_ENV", "prod"}]
    )
  end

  def build_candidate(paths) do
    copy_release_tree(paths.base, paths.candidate)
    old_release = release_dir(paths.candidate, @base_version)
    target_release = release_dir(paths.candidate, @target_version)
    :ok = File.rename(old_release, target_release)

    :ok =
      File.write(
        Path.join(paths.candidate, "releases/start_erl.data"),
        runtime() <> " " <> @target_version <> "\n"
      )

    rewrite_release(Path.join(target_release, "#{@release}.rel"), @target_version)

    old_lib = Path.join(paths.candidate, "lib/arc_net-#{@base_version}")
    target_lib = Path.join(paths.candidate, "lib/arc_net-#{@target_version}")
    :ok = File.rename(old_lib, target_lib)
    rewrite_app(Path.join(target_lib, "ebin/arc_net.app"), @target_version)
    compile_relay(paths.candidate, :target)
    write_appup(Path.join(target_lib, "ebin/arc_net.appup"))
    rewrite_boot_files(target_release, @target_version)
  end

  def build_package(paths) do
    command!(
      "mix",
      [
        "run",
        "--no-start",
        "scripts/build-hot-release.exs",
        "--base",
        paths.base,
        "--candidate",
        paths.candidate,
        "--build",
        @target_build,
        "--os",
        platform_os(),
        "--arch",
        platform_arch(),
        "--output",
        paths.package
      ],
      env: [{"MIX_ENV", "prod"}]
    )
  end

  def publish_channel(paths) do
    identity = Identity.generate()

    digest =
      paths.package
      |> File.read!()
      |> then(&:crypto.hash(:sha256, &1))
      |> Base.encode16(case: :lower)

    provider = paths.provider
    File.mkdir_p!(Path.join(provider, "channels"))
    File.mkdir_p!(Path.join(provider, "blobs"))
    :ok = File.cp(paths.package, Path.join(provider, "blobs/#{digest}.tar.gz"))

    plan =
      paths.package
      |> archive_file("releases/#{@target_version}/relup")
      |> then(&:crypto.hash(:sha256, &1))
      |> Base.encode16(case: :lower)

    unsigned = %{
      "schema_version" => 1,
      "channel" => "stable",
      "publisher" => Identity.encode_public_key(identity),
      "sequence" => 1,
      "expires_at" => System.system_time(:second) + 3_600,
      "releases" => [
        %{
          "version" => @target_version,
          "build" => @target_build,
          "runtime" => runtime(),
          "platform" => %{"os" => platform_os(), "arch" => platform_arch()},
          "size" => File.stat!(paths.package).size,
          "sha256" => digest,
          "sources" => [
            %{
              "build" => @base_build,
              "runtime" => runtime(),
              "upgrade_plan_sha256" => plan,
              "downgrade_plan_sha256" => plan
            }
          ],
          "restart_required" => false,
          "withdrawn" => false,
          "eligible" => true
        }
      ]
    }

    {:ok, manifest} = Manifest.sign(identity, unsigned)
    :ok = File.write(Path.join(provider, "channels/stable.json"), :json.encode(manifest))

    relay_identity = Identity.generate()
    key_name = Identity.name(relay_identity)
    key_dir = Path.join(paths.root, "keys")
    File.mkdir_p!(key_dir)

    :ok =
      File.write(
        Path.join(key_dir, key_name <> ".toml"),
        "[identity]\nseed = \"#{Base.encode16(relay_identity.seed, case: :lower)}\"\n"
      )

    port = unused_port()

    document = %{
      "role" => "relay",
      "key" => key_name,
      "port" => port,
      "peers" => [],
      "transit" => false,
      "update" => %{
        "state_dir" => paths.state,
        "publisher" => Identity.encode_public_key(identity),
        "channel" => "stable",
        "pin" => :null,
        "source" => %{"local_dir" => provider}
      }
    }

    :ok = File.write(paths.config, :json.encode(document))

    paths
    |> Map.put(:port, port)
    |> Map.put(:relay_pubkey, Identity.encode_public_key(relay_identity))
  end

  def publish_federated_channel(paths) do
    publisher = Identity.generate()
    provider_identity = Identity.generate()
    relay_identity = Identity.generate()
    remote_relay_identity = Identity.generate()
    remote_port = unused_port()

    write_release_channel(paths, publisher)

    key_name = Identity.name(relay_identity)
    key_dir = Path.join(paths.root, "keys")
    File.mkdir_p!(key_dir)

    :ok =
      File.write(
        Path.join(key_dir, key_name <> ".toml"),
        "[identity]\nseed = \"#{Base.encode16(relay_identity.seed, case: :lower)}\"\n"
      )

    port = unused_port()

    document = %{
      "role" => "relay",
      "key" => key_name,
      "port" => port,
      "peers" => ["#{Identity.encode_public_key(remote_relay_identity)}@127.0.0.1:#{remote_port}"],
      "transit" => false,
      "update" => %{
        "state_dir" => paths.state,
        "publisher" => Identity.encode_public_key(publisher),
        "channel" => "stable",
        "pin" => :null,
        "source" => %{
          "uri" => "releases+arc://#{Identity.encode_public_key(provider_identity)}/releases",
          "relay" => "127.0.0.1:#{port}",
          "relay_pubkey" => Identity.encode_public_key(relay_identity)
        }
      }
    }

    :ok = File.write(paths.config, :json.encode(document))

    paths
    |> Map.merge(%{
      port: port,
      relay_identity: relay_identity,
      relay_pubkey: Identity.encode_public_key(relay_identity),
      provider_identity: provider_identity,
      remote_relay_identity: remote_relay_identity,
      remote_port: remote_port
    })
  end

  def start_federated_provider(paths) do
    configure_federation_runtime(paths)
    ensure_federation_apps!()
    previous_releases_root = System.get_env("RELEASES_ROOT")
    System.put_env("RELEASES_ROOT", paths.provider)

    {:ok, relay} =
      Arc.Net.Relay.start_link(paths.remote_port,
        relay_identity: paths.remote_relay_identity,
        federation_peers: [
          %{
            public_key: paths.relay_identity.public_key,
            host: ~c"127.0.0.1",
            port: paths.port
          }
        ]
      )

    {:ok, provider} =
      Agent.start_link(paths.provider_identity,
        serve:
          "exec://#{Path.join(@release_provider, "run.sh")}?manifest=#{URI.encode_www_form(Path.join(@release_provider, "manifest.json"))}"
      )

    :ok =
      Arc.Net.connect_relay(
        ~c"127.0.0.1",
        paths.remote_port,
        paths.provider_identity,
        paths.remote_relay_identity.public_key
      )

    :ok = Agent.publish_relay(provider, federation: :network)

    %{
      relay: relay,
      provider: provider,
      provider_identity: paths.provider_identity,
      relay_identity: paths.remote_relay_identity,
      previous_releases_root: previous_releases_root
    }
  end

  def stop_federated_provider(%{
        provider: provider,
        relay: relay,
        previous_releases_root: previous
      }) do
    if Process.alive?(provider), do: GenServer.stop(provider, :normal)
    if Process.alive?(relay), do: GenServer.stop(relay, :normal)
    restore_env("RELEASES_ROOT", previous)
    :ok
  end

  defp write_release_channel(paths, identity) do
    digest =
      paths.package
      |> File.read!()
      |> then(&:crypto.hash(:sha256, &1))
      |> Base.encode16(case: :lower)

    provider = paths.provider
    File.mkdir_p!(Path.join(provider, "channels"))
    File.mkdir_p!(Path.join(provider, "blobs"))
    :ok = File.cp(paths.package, Path.join(provider, "blobs/#{digest}.tar.gz"))

    plan =
      paths.package
      |> archive_file("releases/#{@target_version}/relup")
      |> then(&:crypto.hash(:sha256, &1))
      |> Base.encode16(case: :lower)

    unsigned = %{
      "schema_version" => 1,
      "channel" => "stable",
      "publisher" => Identity.encode_public_key(identity),
      "sequence" => 1,
      "expires_at" => System.system_time(:second) + 3_600,
      "releases" => [
        %{
          "version" => @target_version,
          "build" => @target_build,
          "runtime" => runtime(),
          "platform" => %{"os" => platform_os(), "arch" => platform_arch()},
          "size" => File.stat!(paths.package).size,
          "sha256" => digest,
          "sources" => [
            %{
              "build" => @base_build,
              "runtime" => runtime(),
              "upgrade_plan_sha256" => plan,
              "downgrade_plan_sha256" => plan
            }
          ],
          "restart_required" => false,
          "withdrawn" => false,
          "eligible" => true
        }
      ]
    }

    {:ok, manifest} = Manifest.sign(identity, unsigned)
    :ok = File.write(Path.join(provider, "channels/stable.json"), :json.encode(manifest))
  end

  def run_service_update(paths, opts \\ []) do
    port = start_service(paths)
    Process.put(:managed_update_proof_port, port)
    socket = Path.join(paths.state, "admin.sock")
    wait_socket(socket)

    federation_link =
      case Keyword.get(opts, :federation) do
        nil ->
          nil

        %{relay: remote_relay} ->
          wait_federation_link(remote_relay, paths.relay_identity.public_key)
      end

    traffic =
      case Keyword.get(opts, :federation) do
        nil -> nil
        federation -> start_federation_traffic(paths, federation)
      end

    status = update(paths, "status")
    assert(status["service"] == "relay", "managed service did not report relay status")
    assert(status["mode"] == "notify", "managed service did not retain notify policy")
    assert(status["running"]["build"] == @base_build, "base release metadata was not active")

    _ = update(paths, "check")

    available = wait_status(paths, fn status -> status["state"] == "available" end)
    assert(available["eligible"]["build"] == @target_build, "signed target was not eligible")

    _ = update(paths, "apply")

    current = wait_status(paths, fn status -> status["state"] == "current" end, 20_000)
    assert(current["build"] == @target_build, "operator apply did not commit target build")

    assert(
      get_in(current, ["observation", "verdict"]) == "passed",
      "traffic observer did not pass"
    )

    assert(
      get_in(current, ["observation", "messages", "alice_to_bob"]) >= 1,
      "missing forward probe"
    )

    assert(
      get_in(current, ["observation", "messages", "bob_to_alice"]) >= 1,
      "missing reverse probe"
    )

    case {Keyword.get(opts, :federation), federation_link} do
      {%{relay: remote_relay}, link} when is_pid(link) ->
        assert(Process.alive?(link), "remote federation connection died during hot update")

        assert(
          federation_link_pid(remote_relay, paths.relay_identity.public_key) == link,
          "remote federation connection was replaced during hot update"
        )

      _ ->
        :ok
    end

    verify_federation_traffic(traffic)

    stop_service(port)
    Process.delete(:managed_update_proof_port)
    wait_absent(socket)
    restarted_port = start_service(paths)
    Process.put(:managed_update_proof_port, restarted_port)
    wait_socket(socket)

    restarted =
      wait_status(paths, fn status -> get_in(status, ["running", "build"]) == @target_build end)

    assert(
      restarted["running"]["version"] == @target_version,
      "restart did not boot permanent target"
    )

    relay = relay_status(paths)

    assert(
      relay["version"] == @target_version,
      "restarted relay still loaded the old arc_net application"
    )
  end

  def start_service(paths) do
    Port.open({:spawn_executable, String.to_charlist(Path.join(paths.base, "bin/arc"))}, [
      :binary,
      :stderr_to_stdout,
      :exit_status,
      args: [~c"service", ~c"start", ~c"--config", String.to_charlist(paths.config)]
    ])
  end

  def stop_service(port) when is_port(port) do
    case Port.info(port, :os_pid) do
      {:os_pid, pid} when is_integer(pid) and pid > 0 ->
        _ = System.cmd("kill", ["-TERM", Integer.to_string(pid)], stderr_to_stdout: true)
        await_port_exit(port)

      _ ->
        :ok
    end
  end

  def stop_service(_), do: :ok

  def update(paths, operation) do
    {output, 0} =
      System.cmd(
        Path.join(paths.base, "bin/arc"),
        ["update", operation, "--socket", Path.join(paths.state, "admin.sock")],
        stderr_to_stdout: true
      )

    :json.decode(output)
  end

  def relay_status(paths) do
    {output, 0} =
      System.cmd(
        Path.join(paths.base, "bin/arc"),
        [
          "status",
          "--format",
          "json",
          "--relay",
          "127.0.0.1:#{paths.port}",
          "--relay-pubkey",
          paths.relay_pubkey
        ],
        stderr_to_stdout: true
      )

    :json.decode(output)
  end

  def wait_socket(socket, attempts \\ 100)

  def wait_socket(_socket, 0) do
    output = drain_port(Process.get(:managed_update_proof_port), [])
    raise("managed service did not create its admin socket: #{inspect(output)}")
  end

  def wait_socket(socket, attempts) do
    if File.exists?(socket) do
      :ok
    else
      Process.sleep(50)
      wait_socket(socket, attempts - 1)
    end
  end

  def wait_absent(path, attempts \\ 100)
  def wait_absent(_path, 0), do: raise("managed service left its private admin socket behind")

  def wait_absent(path, attempts) do
    if File.exists?(path) do
      Process.sleep(50)
      wait_absent(path, attempts - 1)
    else
      :ok
    end
  end

  def wait_status(paths, predicate, timeout \\ 8_000) do
    deadline = System.monotonic_time(:millisecond) + timeout
    wait_status_until(paths, predicate, deadline)
  end

  defp wait_status_until(paths, predicate, deadline) do
    status = update(paths, "status")

    cond do
      predicate.(status) ->
        status

      System.monotonic_time(:millisecond) >= deadline ->
        raise("timed out waiting for #{inspect(status)}")

      true ->
        Process.sleep(100)
        wait_status_until(paths, predicate, deadline)
    end
  end

  defp wait_federation_link(relay, peer, attempts \\ 200)

  defp wait_federation_link(_relay, _peer, 0),
    do: raise("approved federation link did not become ready")

  defp wait_federation_link(relay, peer, attempts) do
    case federation_link_pid(relay, peer) do
      pid when is_pid(pid) ->
        pid

      _ ->
        Process.sleep(50)
        wait_federation_link(relay, peer, attempts - 1)
    end
  end

  defp federation_link_pid(relay, peer) do
    with %{federation: federation} when is_pid(federation) <- :sys.get_state(relay),
         %{links: links} <- :sys.get_state(federation),
         %{stage: :ready, conn: conn} when is_pid(conn) <- Map.get(links, peer),
         true <- Process.alive?(conn) do
      conn
    else
      _ -> nil
    end
  end

  defp ensure_federation_apps! do
    for app <- [:arc_net, :arc_data] do
      case Application.ensure_all_started(app) do
        {:ok, _} ->
          :ok

        {:error, reason} ->
          raise("could not start #{app} for federation proof: #{inspect(reason)}")
      end
    end
  end

  defp configure_federation_runtime(paths) do
    Application.put_all_env(
      arc_identity: [
        keys_dir: Path.join(paths.root, "federation-keys"),
        default_file: Path.join(paths.root, "federation-default.key")
      ],
      arc_control: [control_dir: Path.join(paths.root, "federation-control")],
      arc_data: [mailbox_dir: Path.join(paths.root, "federation-mailbox")]
    )
  end

  defp restore_env(name, nil), do: System.delete_env(name)
  defp restore_env(name, value), do: System.put_env(name, value)

  defp start_federation_traffic(paths, %{provider: remote, provider_identity: remote_identity}) do
    identity = Identity.generate()
    {:ok, citizen} = Agent.start_link(identity)

    :ok =
      Arc.Net.connect_relay(
        ~c"127.0.0.1",
        paths.port,
        identity,
        paths.relay_identity.public_key
      )

    :ok = Agent.publish_relay(citizen)
    remote_key = remote_identity.public_key
    {:ok, _entry} = Agent.connect(citizen, Identity.encode_public_key(remote_key), 15_000)
    {:ok, counter} = Elixir.Agent.start_link(fn -> %{sent: 0, failed: 0} end)

    worker =
      spawn(fn ->
        send_federation_events(citizen, remote_key, counter, 1)
      end)

    %{
      citizen: citizen,
      counter: counter,
      worker: worker,
      remote: remote,
      identity: identity,
      endpoint: relay_endpoint!(identity.public_key)
    }
  end

  defp send_federation_events(citizen, remote_key, counter, sequence) do
    result =
      Agent.emit_event(
        citizen,
        remote_key,
        "managed-update-federation-proof",
        Integer.to_string(sequence)
      )

    Elixir.Agent.update(counter, fn state ->
      case result do
        :ok -> %{state | sent: state.sent + 1}
        _ -> %{state | failed: state.failed + 1}
      end
    end)

    receive do
      :stop -> :ok
    after
      25 -> send_federation_events(citizen, remote_key, counter, sequence + 1)
    end
  end

  defp verify_federation_traffic(nil), do: :ok

  defp verify_federation_traffic(%{
         citizen: citizen,
         counter: counter,
         worker: worker,
         remote: remote,
         identity: identity,
         endpoint: endpoint
       }) do
    ref = Process.monitor(worker)
    if Process.alive?(worker), do: send(worker, :stop)
    await_worker_stop(ref)

    state = Elixir.Agent.get(counter, & &1)

    expected = Enum.map(1..state.sent, &Integer.to_string/1)
    received = await_federation_events(remote, expected, [])

    try do
      assert(state.failed == 0, "federation traffic failed while the relay upgraded")
      assert(state.sent >= 3, "federation traffic did not span the update")
      assert(received == expected, "federation traffic was lost, duplicated, or reordered")

      assert(
        Arc.Net.relay_endpoint_current?(identity.public_key, endpoint),
        "citizen relay connection was replaced during hot update"
      )
    after
      if Process.alive?(citizen), do: GenServer.stop(citizen, :normal)
      if Process.alive?(counter), do: Elixir.Agent.stop(counter, :normal)
    end
  end

  defp await_worker_stop(ref) do
    receive do
      {:DOWN, ^ref, :process, _worker, :normal} ->
        :ok

      {:DOWN, ^ref, :process, _worker, reason} ->
        raise("federation traffic worker stopped: #{inspect(reason)}")
    after
      2_000 -> raise("federation traffic worker did not stop")
    end
  end

  defp await_federation_events(remote, expected, received, attempts \\ 50)

  defp await_federation_events(_remote, _expected, received, 0), do: received

  defp await_federation_events(remote, expected, received, attempts) do
    latest =
      remote
      |> Agent.read_inbox()
      |> Enum.filter(
        &(&1.kind == :event and &1.meta["topic"] == "managed-update-federation-proof")
      )
      |> Enum.map(& &1.text)

    received = received ++ latest

    if received == expected do
      received
    else
      Process.sleep(50)
      await_federation_events(remote, expected, received, attempts - 1)
    end
  end

  defp relay_endpoint!(public_key) do
    case Arc.Net.relay_endpoint(public_key) do
      {:ok, %{connection: connection}} when is_pid(connection) -> connection
      other -> raise("federation proof could not inspect citizen connection: #{inspect(other)}")
    end
  end

  defp rewrite_release(path, target) do
    {:ok, [{:release, {name, _version}, erts, applications}]} =
      :file.consult(String.to_charlist(path))

    applications =
      Enum.map(applications, fn
        {:arc_net, _version, type} -> {:arc_net, String.to_charlist(target), type}
        {:arc_net, _version} -> {:arc_net, String.to_charlist(target)}
        entry -> entry
      end)

    File.write!(
      path,
      :io_lib.format("~p.~n", [{:release, {name, String.to_charlist(target)}, erts, applications}])
    )
  end

  defp rewrite_boot_files(release_dir, target) do
    Enum.each(["start", "start_clean"], fn name ->
      script_path = Path.join(release_dir, name <> ".script")
      boot_path = Path.join(release_dir, name <> ".boot")
      {:ok, [script]} = :file.consult(String.to_charlist(script_path))
      rewritten = rewrite_boot_term(script, target)
      File.write!(script_path, :io_lib.format("~p.~n", [rewritten]))
      File.write!(boot_path, :erlang.term_to_binary(rewritten))
    end)
  end

  defp rewrite_boot_term({:script, {name, _version}, instructions}, target) do
    {:script, {name, String.to_charlist(target)}, rewrite_boot_term(instructions, target)}
  end

  defp rewrite_boot_term({:application, :arc_net, properties}, target) when is_list(properties) do
    {:application, :arc_net,
     Enum.map(properties, fn
       {:vsn, _version} -> {:vsn, String.to_charlist(target)}
       entry -> rewrite_boot_term(entry, target)
     end)}
  end

  defp rewrite_boot_term(tuple, target) when is_tuple(tuple) do
    tuple |> Tuple.to_list() |> Enum.map(&rewrite_boot_term(&1, target)) |> List.to_tuple()
  end

  defp rewrite_boot_term(list, target) when is_list(list) do
    if Enum.all?(list, &is_integer/1) do
      list
      |> List.to_string()
      |> String.replace("arc_net-#{@base_version}", "arc_net-#{target}")
      |> String.to_charlist()
    else
      Enum.map(list, &rewrite_boot_term(&1, target))
    end
  end

  defp rewrite_boot_term(value, _target), do: value

  defp rewrite_app(path, target) do
    {:ok, [{:application, :arc_net, properties}]} = :file.consult(String.to_charlist(path))

    File.write!(
      path,
      :io_lib.format("~p.~n", [
        {:application, :arc_net, Keyword.put(properties, :vsn, String.to_charlist(target))}
      ])
    )
  end

  defp write_appup(path) do
    relay = Arc.Net.Relay

    appup =
      {String.to_charlist(@target_version),
       [
         {String.to_charlist(@base_version),
          [{:update, relay, {:advanced, []}, :soft_purge, :soft_purge, []}]}
       ],
       [
         {String.to_charlist(@base_version),
          [{:update, relay, {:advanced, []}, :soft_purge, :soft_purge, []}]}
       ]}

    File.write!(path, :io_lib.format("~p.~n", [appup]))
  end

  defp compile_relay(root, version) do
    source = Path.join(root, "relay_#{version}.ex")

    ebin =
      Path.join(
        root,
        "lib/arc_net-#{if(version == :base, do: @base_version, else: @target_version)}/ebin"
      )

    File.write!(source, relay_source(version))

    paths =
      Path.wildcard(Path.join(root, "lib/*/ebin"))
      |> Enum.flat_map(&["-pa", &1])

    command!(
      System.find_executable("elixirc") || raise("elixirc is unavailable"),
      paths ++ ["-o", ebin, source]
    )
  end

  defp relay_source(version) do
    source = Path.expand("../../apps/arc_net/lib/arc/net/relay.ex", __DIR__) |> File.read!()
    {:defmodule, meta, [name, [do: body]]} = Code.string_to_quoted!(source, columns: true)

    additions =
      quote do
        @impl GenServer
        def code_change({:down, _}, state, _extra),
          do: {:ok, Map.delete(state, :managed_update_proof)}

        def code_change(_old, state, _extra), do: {:ok, state}
      end

    additions =
      if version == :target do
        quote do
          @impl GenServer
          def code_change({:down, _}, state, _extra),
            do: {:ok, Map.delete(state, :managed_update_proof)}

          def code_change(_old, state, _extra),
            do: {:ok, Map.put(state, :managed_update_proof, :v2)}
        end
      else
        additions
      end

    appended =
      case body do
        {:__block__, block_meta, expressions} ->
          {:__block__, block_meta, expressions ++ block_expressions(additions)}

        expression ->
          {:__block__, [], [expression | block_expressions(additions)]}
      end

    Macro.to_string({:defmodule, meta, [name, [do: appended]]})
  end

  defp block_expressions({:__block__, _, expressions}), do: expressions
  defp block_expressions(expression), do: [expression]

  defp isolate_runtime(paths) do
    runtime = Path.join(release_dir(paths.base, @base_version), "runtime.exs")

    additions = """

    # Test-only isolation for scripts/test-managed-update.exs.
    config :arc_identity, keys_dir: #{inspect(Path.join(paths.root, "keys"))}, default_file: #{inspect(Path.join(paths.root, "default.key"))}
    config :arc_control, control_dir: #{inspect(Path.join(paths.root, "control"))}
    config :arc_data, mailbox_dir: #{inspect(Path.join(paths.root, "mailbox"))}
    """

    File.write!(runtime, File.read!(runtime) <> additions)
  end

  defp copy_release_tree(source, destination) do
    File.mkdir_p!(destination)
    copy_tree(source, destination, "")
  end

  defp copy_tree(source, destination, relative) do
    source
    |> File.ls!()
    |> Enum.each(fn name ->
      child_relative = Path.join(relative, name)

      unless child_relative == "releases/COOKIE" do
        from = Path.join(source, name)
        to = Path.join(destination, name)

        case File.lstat!(from) do
          %{type: :directory} ->
            File.mkdir!(to)
            copy_tree(from, to, child_relative)

          %{type: :regular, mode: mode} ->
            :ok = File.cp(from, to)
            :ok = File.chmod(to, Bitwise.band(mode, 0o777))

          stat ->
            raise("unsafe release fixture entry: #{child_relative} (#{inspect(stat.type)})")
        end
      end
    end)
  end

  defp archive_file(archive, name) do
    case :erl_tar.extract(String.to_charlist(archive), [
           :compressed,
           :memory,
           {:files, [String.to_charlist(name)]}
         ]) do
      {:ok, [{_name, bytes}]} when is_binary(bytes) -> bytes
      other -> raise("could not read #{name} from update archive: #{inspect(other)}")
    end
  end

  defp await_port_exit(port) do
    receive do
      {^port, {:exit_status, _}} -> :ok
      {^port, _output} -> await_port_exit(port)
    after
      5_000 -> raise("managed service did not stop after its owned process was terminated")
    end
  end

  defp drain_port(port, acc) when is_port(port) and length(acc) < 8 do
    receive do
      {^port, {:data, bytes}} -> drain_port(port, [bytes | acc])
      {^port, {:exit_status, status}} -> Enum.reverse([{:exit_status, status} | acc])
    after
      0 -> Enum.reverse(acc)
    end
  end

  defp drain_port(_, acc), do: Enum.reverse(acc)

  def command!(command, args, opts \\ []) do
    {output, status} = System.cmd(command, args, Keyword.merge([stderr_to_stdout: true], opts))

    if status == 0,
      do: output,
      else: raise("command failed: #{command} #{Enum.join(args, " ")}\n#{output}")
  end

  defp release_dir(root, version), do: Path.join(root, "releases/#{version}")
  defp runtime, do: :erlang.system_info(:version) |> to_string()
  defp platform_os, do: :os.type() |> elem(1) |> Atom.to_string()

  defp platform_arch,
    do: :erlang.system_info(:system_architecture) |> to_string() |> String.split("-") |> hd()

  defp unused_port do
    {:ok, socket} = :gen_tcp.listen(0, [:binary, active: false])
    {:ok, port} = :inet.port(socket)
    :ok = :gen_tcp.close(socket)
    port
  end

  def make_root do
    # macOS's per-user temporary directory can exceed Unix socket path limits.
    temporary = if :os.type() == {:unix, :darwin}, do: "/tmp", else: System.tmp_dir!()

    root =
      Path.join(
        temporary,
        "arc-upd-" <> Base.encode16(:crypto.strong_rand_bytes(8), case: :lower)
      )

    case File.mkdir(root) do
      :ok ->
        :ok = File.chmod(root, 0o700)
        root

      {:error, :eexist} ->
        make_root()

      {:error, reason} ->
        raise("could not create proof root: #{inspect(reason)}")
    end
  end

  def remove_root(root) do
    case File.rm_rf(root) do
      {:ok, _} -> :ok
      {:error, reason, _} -> raise("could not remove own proof root: #{inspect(reason)}")
    end
  end

  defp assert(true, _message), do: :ok
  defp assert(false, message), do: raise(message)
end
