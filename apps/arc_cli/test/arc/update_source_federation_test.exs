defmodule Arc.CLI.Update.SourceFederationTest do
  use ExUnit.Case, async: false

  alias Arc.CLI.Update.Source
  alias Arc.Data.Agent
  alias Arc.Identity

  @project_root Path.expand("../../../..", __DIR__)
  @child_script Path.join(@project_root, "test/fixtures/federation/arc_child.exs")
  @bundle Path.expand("../../../../providers/releases", __DIR__)

  setup do
    :ok = Arc.Control.Local.reset()
    :ok = Arc.Net.TransportManager.reset()

    root =
      Path.join(System.tmp_dir!(), "arc-update-federation-#{System.unique_integer([:positive])}")

    releases = Path.join(root, "releases")
    staging = Path.join(root, "staging")
    File.mkdir_p!(Path.join(releases, "channels"))
    File.mkdir_p!(Path.join(releases, "blobs"))
    File.mkdir_p!(staging)

    previous_root = System.get_env("RELEASES_ROOT")
    System.put_env("RELEASES_ROOT", releases)

    on_exit(fn ->
      restore_env("RELEASES_ROOT", previous_root)
      Arc.Net.TransportManager.reset()
      Arc.Control.Local.reset()
      File.rm_rf!(root)
    end)

    %{root: root, releases: releases, staging: staging}
  end

  test "fetches channel and archive across two approved federation edges", ctx do
    network = start_network(:network, true)
    on_exit(fn -> stop_network(network) end)

    channel = "{\"channel\":\"stable\",\"sequence\":1}"
    bytes = :crypto.strong_rand_bytes(500_000)
    digest = digest(bytes)
    write_release(ctx.releases, channel, digest, bytes)

    source = source(network)

    assert_eventually(fn ->
      Source.channel(source, "stable", deadline_ms: 5_000) == {:ok, channel}
    end)

    destination = Path.join(ctx.staging, digest <> ".tar.gz")

    assert {:ok, ^destination} =
             Source.stage_archive(source, digest, byte_size(bytes), destination,
               deadline_ms: 10_000,
               request_timeout_ms: 3_000
             )

    assert File.read!(destination) == bytes
  end

  test "refuses a provider that did not opt into federation", ctx do
    network = start_network(:local, true)
    on_exit(fn -> stop_network(network) end)
    bytes = :crypto.strong_rand_bytes(64)
    write_release(ctx.releases, "{\"channel\":\"stable\"}", digest(bytes), bytes)
    assert_provider_available_on_middle_relay(network)
    assert_provider_available_on_home_relay(network)

    assert {:error, _} =
             Source.channel(source(network), "stable",
               deadline_ms: 3_000,
               request_timeout_ms: 500
             )
  end

  test "refuses onward lookup when the middle relay lacks transit consent", ctx do
    network = start_network(:network, false)
    on_exit(fn -> stop_network(network) end)

    bytes = :crypto.strong_rand_bytes(64)
    write_release(ctx.releases, "{\"channel\":\"stable\"}", digest(bytes), bytes)
    assert_provider_available_on_middle_relay(network)

    assert {:error, _} =
             Source.channel(source(network), "stable",
               deadline_ms: 3_000,
               request_timeout_ms: 500
             )
  end

  test "interrupted federation archive transfer leaves no completed artifact", ctx do
    network = start_network(:network, true)
    on_exit(fn -> stop_network(network) end)

    bytes = :crypto.strong_rand_bytes(8 * 1024 * 1024)
    digest = digest(bytes)
    write_release(ctx.releases, "{\"channel\":\"stable\"}", digest, bytes)
    source = source(network)

    assert_eventually(fn ->
      match?({:ok, _}, Source.channel(source, "stable", deadline_ms: 5_000))
    end)

    destination = Path.join(ctx.staging, digest <> ".tar.gz")

    task =
      Task.async(fn ->
        Source.stage_archive(source, digest, byte_size(bytes), destination,
          deadline_ms: 15_000,
          request_timeout_ms: 2_000
        )
      end)

    wait_for_partial_archive(task, destination, byte_size(bytes), 10_000)
    :ok = stop_child(network.b)

    assert {:error, _} = Task.await(task, 6_000)
    refute File.exists?(destination)

    refute Path.wildcard(Path.join(ctx.staging, ".#{Path.basename(destination)}.*.part"),
             match_dot: true
           ) != []
  end

  defp start_network(scope, transit?) do
    root =
      Path.join(
        System.tmp_dir!(),
        "arc-update-source-network-#{System.unique_integer([:positive])}"
      )

    File.mkdir_p!(root)
    File.chmod!(root, 0o700)
    a_state = Path.join(root, "a")
    b_state = Path.join(root, "b")
    c_state = Path.join(root, "c")
    a_identity = create_identity(a_state)
    b_identity = create_identity(b_state)
    c_identity = create_identity(c_state)
    a_port = unused_port()
    b_port = unused_port()
    c_port = unused_port()

    b =
      start_relay(
        b_state,
        b_identity,
        b_port,
        [{a_identity, a_port}, {c_identity, c_port}],
        transit?
      )

    a = start_relay(a_state, a_identity, a_port, [{b_identity, b_port}], false)
    c = start_relay(c_state, c_identity, c_port, [{b_identity, b_port}], false)

    citizen_identity = Identity.generate()
    home_citizen_identity = Identity.generate()
    provider_identity = Identity.generate()
    home_probe_identity = Identity.generate()
    middle_probe_identity = Identity.generate()
    {:ok, citizen} = Agent.start_link(citizen_identity)
    {:ok, home_citizen} = Agent.start_link(home_citizen_identity)

    {:ok, provider} =
      Agent.start_link(provider_identity,
        serve:
          "exec://#{Path.join(@bundle, "run.sh")}?manifest=#{URI.encode_www_form(Path.join(@bundle, "manifest.json"))}"
      )

    {:ok, home_probe} = Agent.start_link(home_probe_identity, serve: provider_uri())
    {:ok, middle_probe} = Agent.start_link(middle_probe_identity, serve: provider_uri())

    :ok =
      Arc.Net.connect_relay(
        ~c"127.0.0.1",
        a_port,
        citizen_identity,
        decode_key(a_identity.public_key)
      )

    :ok =
      Arc.Net.connect_relay(
        ~c"127.0.0.1",
        c_port,
        home_citizen_identity,
        decode_key(c_identity.public_key)
      )

    :ok =
      Arc.Net.connect_relay(
        ~c"127.0.0.1",
        c_port,
        provider_identity,
        decode_key(c_identity.public_key)
      )

    :ok =
      Arc.Net.connect_relay(
        ~c"127.0.0.1",
        c_port,
        home_probe_identity,
        decode_key(c_identity.public_key)
      )

    :ok =
      Arc.Net.connect_relay(
        ~c"127.0.0.1",
        b_port,
        middle_probe_identity,
        decode_key(b_identity.public_key)
      )

    :ok = Agent.publish_relay(citizen)
    :ok = Agent.publish_relay(home_citizen)
    :ok = Agent.publish_relay(provider, federation: scope)
    :ok = Agent.publish_relay(home_probe, federation: :network)
    :ok = Agent.publish_relay(middle_probe, federation: :network)

    %{
      a: a,
      b: b,
      c: c,
      root: root,
      a_identity: a_identity,
      b_identity: b_identity,
      c_identity: c_identity,
      citizen: citizen,
      home_citizen: home_citizen,
      provider: provider,
      provider_identity: provider_identity,
      home_probe: home_probe,
      home_probe_identity: home_probe_identity,
      middle_probe: middle_probe,
      middle_probe_identity: middle_probe_identity
    }
  end

  defp stop_network(network) do
    for process <- [
          network.citizen,
          network.home_citizen,
          network.provider,
          network.home_probe,
          network.middle_probe
        ],
        is_pid(process) do
      # The agents are linked to the test process, which can still be exiting
      # here. An agent that dies during the stop is already stopped.
      try do
        GenServer.stop(process, :normal)
      catch
        :exit, _ -> :ok
      end
    end

    Enum.each([network.a, network.b, network.c], &stop_child/1)
    File.rm_rf!(network.root)
  end

  defp source(network) do
    {:arc, network.citizen,
     "releases+arc://#{Identity.encode_public_key(network.provider_identity)}/releases"}
  end

  defp assert_provider_available_on_home_relay(network) do
    assert_eventually(fn ->
      match?({:ok, _}, Source.channel(home_probe_source(network), "stable", deadline_ms: 5_000))
    end)
  end

  defp assert_provider_available_on_middle_relay(network) do
    assert_eventually(fn ->
      match?({:ok, _}, Source.channel(middle_probe_source(network), "stable", deadline_ms: 5_000))
    end)
  end

  defp home_probe_source(network) do
    {:arc, network.home_citizen,
     "releases+arc://#{Identity.encode_public_key(network.home_probe_identity)}/releases"}
  end

  defp middle_probe_source(network) do
    {:arc, network.citizen,
     "releases+arc://#{Identity.encode_public_key(network.middle_probe_identity)}/releases"}
  end

  defp write_release(root, channel, digest, bytes) do
    File.write!(Path.join([root, "channels", "stable.json"]), channel)
    File.write!(Path.join([root, "blobs", digest <> ".tar.gz"]), bytes)
  end

  defp digest(bytes), do: :crypto.hash(:sha256, bytes) |> Base.encode16(case: :lower)

  defp assert_eventually(check, timeout_ms \\ 10_000) do
    deadline = System.monotonic_time(:millisecond) + timeout_ms
    await_eventually(check, deadline)
  end

  defp await_eventually(check, deadline) do
    if check.() do
      :ok
    else
      if System.monotonic_time(:millisecond) >= deadline do
        flunk("federated source did not become available")
      end

      Process.sleep(100)
      await_eventually(check, deadline)
    end
  end

  defp wait_for_partial_archive(task, destination, total, timeout_ms) do
    deadline = System.monotonic_time(:millisecond) + timeout_ms
    pattern = Path.join(Path.dirname(destination), ".#{Path.basename(destination)}.*.part")
    await_partial_archive(task, pattern, total, deadline)
  end

  defp await_partial_archive(task, pattern, total, deadline) do
    case Path.wildcard(pattern, match_dot: true) do
      [path] ->
        case File.stat(path) do
          {:ok, %{size: size}} when size > 0 and size < total -> :ok
          _ -> retry_partial_archive(task, pattern, total, deadline)
        end

      _ ->
        retry_partial_archive(task, pattern, total, deadline)
    end
  end

  defp retry_partial_archive(task, pattern, total, deadline) do
    case Task.yield(task, 0) do
      {:ok, result} -> flunk("archive transfer ended before staging bytes: #{inspect(result)}")
      nil -> :ok
    end

    if System.monotonic_time(:millisecond) >= deadline do
      flunk("archive transfer never reached a partial staged file")
    end

    Process.sleep(25)
    await_partial_archive(task, pattern, total, deadline)
  end

  defp unused_port do
    {:ok, socket} = :gen_tcp.listen(0, [:binary, active: false])
    {:ok, port} = :inet.port(socket)
    :ok = :gen_tcp.close(socket)
    port
  end

  defp create_identity(state_root) do
    result = run_child(state_root, ["keys", "gen"])
    assert result.status == 0, result.output

    [_, name] = Regex.run(~r/^\s*name:\s+(\S+)$/m, result.output)
    [_, public_key] = Regex.run(~r/^\s*public_key:\s+([a-f0-9]{64})$/m, result.output)
    %{name: name, public_key: public_key}
  end

  defp start_relay(state_root, identity, port, peers, transit?) do
    peer_args =
      Enum.flat_map(peers, fn {peer, peer_port} ->
        ["--peer", "#{peer.public_key}@127.0.0.1:#{peer_port}"]
      end)

    args = ["relay", "--port", Integer.to_string(port), "--key", identity.name] ++ peer_args
    child = open_child(state_root, if(transit?, do: args ++ ["--transit"], else: args))
    assert_ready(child, "arc relay listening on port #{port}")
    child
  end

  defp provider_uri do
    "exec://#{Path.join(@bundle, "run.sh")}?manifest=#{URI.encode_www_form(Path.join(@bundle, "manifest.json"))}"
  end

  defp decode_key(key) do
    {:ok, decoded} = Base.decode16(key, case: :lower)
    decoded
  end

  defp run_child(state_root, argv) do
    child = open_child(state_root, argv)
    await_child(child, System.monotonic_time(:millisecond) + 15_000, "")
  end

  defp open_child(state_root, argv) do
    mix = System.find_executable("mix") || raise("mix executable is unavailable")
    args = ["run", "--no-compile", "--no-start", @child_script, "--", state_root | argv]

    Port.open({:spawn_executable, String.to_charlist(mix)}, [
      :binary,
      :exit_status,
      :use_stdio,
      :stderr_to_stdout,
      args: Enum.map(args, &String.to_charlist/1),
      cd: String.to_charlist(@project_root),
      env: [{~c"MIX_ENV", ~c"test"}, {~c"ARC_RELAY_MAX_FRAME_BYTES", ~c"6291456"}]
    ])
  end

  defp assert_ready(child, expected) do
    await_ready(child, expected, System.monotonic_time(:millisecond) + 15_000, "")
  end

  defp await_ready(child, expected, deadline, output) do
    receive do
      {^child, {:data, chunk}} ->
        output = output <> chunk

        if String.contains?(output, expected),
          do: :ok,
          else: await_ready(child, expected, deadline, output)

      {^child, {:exit_status, status}} ->
        flunk("relay exited before readiness (#{status}): #{output}")
    after
      100 ->
        if System.monotonic_time(:millisecond) >= deadline,
          do: flunk("relay did not become ready: #{output}"),
          else: await_ready(child, expected, deadline, output)
    end
  end

  defp await_child(child, deadline, output) do
    receive do
      {^child, {:data, chunk}} -> await_child(child, deadline, output <> chunk)
      {^child, {:exit_status, status}} -> %{status: status, output: output}
    after
      100 ->
        if System.monotonic_time(:millisecond) >= deadline,
          do: flunk("child timed out: #{output}"),
          else: await_child(child, deadline, output)
    end
  end

  defp stop_child(child) do
    # An on_exit callback does not own the port and receives none of its
    # messages. A port monitor reports the close to any process.
    ref = Port.monitor(child)

    try do
      Port.command(child, "shutdown\n")
    rescue
      ArgumentError -> :ok
    end

    await_stopped(child, ref, System.monotonic_time(:millisecond) + 10_000)
  end

  defp await_stopped(child, ref, deadline) do
    receive do
      {^child, {:data, _chunk}} ->
        await_stopped(child, ref, deadline)

      {^child, {:exit_status, _status}} ->
        Process.demonitor(ref, [:flush])
        :ok

      {:DOWN, ^ref, :port, ^child, _reason} ->
        :ok
    after
      max(deadline - System.monotonic_time(:millisecond), 0) ->
        flunk("child ignored shutdown request")
    end
  end

  defp restore_env(name, nil), do: System.delete_env(name)
  defp restore_env(name, value), do: System.put_env(name, value)
end
