defmodule Arc.CLI.CatalogFederationIntegrationTest do
  use ExUnit.Case, async: false

  @project_root Path.expand("../../../..", __DIR__)
  @child_script Path.join(@project_root, "test/fixtures/federation/arc_child.exs")
  @probe_script Path.join(@project_root, "test/fixtures/catalog_federation/probe.exs")
  @provider_bundle Path.expand("../../../../providers/files", __DIR__)
  @timeout 30_000

  setup do
    root =
      Path.join(System.tmp_dir!(), "arc-catalog-federation-#{System.unique_integer([:positive])}")

    File.mkdir_p!(root)
    on_exit(fn -> File.rm_rf!(root) end)

    %{
      root: root,
      relay_a_state: Path.join(root, "relay-a"),
      relay_b_state: Path.join(root, "relay-b"),
      relay_c_state: Path.join(root, "relay-c"),
      relay_d_state: Path.join(root, "relay-d"),
      citizen_a_state: Path.join(root, "citizen-a"),
      provider_network_state: Path.join(root, "provider-network"),
      provider_direct_state: Path.join(root, "provider-direct"),
      provider_local_state: Path.join(root, "provider-local"),
      network_storage: Path.join(root, "network-storage"),
      direct_storage: Path.join(root, "direct-storage"),
      local_storage: Path.join(root, "local-storage"),
      relay_a_port: unused_tcp_port(),
      relay_b_port: unused_tcp_port(),
      relay_c_port: unused_tcp_port(),
      relay_d_port: unused_tcp_port()
    }
  end

  test "catalog snapshots and deltas reach a distant relay and withdrawals survive a peer epoch",
       ctx do
    relay_a_identity = create_identity(ctx.relay_a_state)
    relay_b_identity = create_identity(ctx.relay_b_state)
    relay_c_identity = create_identity(ctx.relay_c_state)
    citizen_a_identity = create_identity(ctx.citizen_a_state)
    provider_network_identity = create_identity(ctx.provider_network_state)
    provider_direct_identity = create_identity(ctx.provider_direct_state)
    provider_local_identity = create_identity(ctx.provider_local_state)

    relay_a =
      start_relay(ctx.relay_a_state, relay_a_identity, ctx.relay_a_port,
        peers: [{relay_b_identity, ctx.relay_b_port}]
      )

    relay_b =
      start_relay(ctx.relay_b_state, relay_b_identity, ctx.relay_b_port,
        peers: [{relay_a_identity, ctx.relay_a_port}, {relay_c_identity, ctx.relay_c_port}],
        transit?: true
      )

    relay_c =
      start_relay(ctx.relay_c_state, relay_c_identity, ctx.relay_c_port,
        peers: [{relay_b_identity, ctx.relay_b_port}]
      )

    on_exit(fn -> stop_and_assert_child(relay_c) end)
    on_exit(fn -> stop_and_assert_child(relay_b) end)
    on_exit(fn -> stop_and_assert_child(relay_a) end)

    assert_ready(relay_a.port, "arc relay listening on port #{ctx.relay_a_port}")
    assert_ready(relay_b.port, "arc relay listening on port #{ctx.relay_b_port}")
    assert_ready(relay_c.port, "arc relay listening on port #{ctx.relay_c_port}")

    provider_network =
      start_provider(
        ctx.provider_network_state,
        ctx.network_storage,
        ctx.relay_c_port,
        relay_c_identity.public_key,
        :network
      )

    provider_direct =
      start_provider(
        ctx.provider_direct_state,
        ctx.direct_storage,
        ctx.relay_c_port,
        relay_c_identity.public_key,
        :direct
      )

    provider_local =
      start_provider(
        ctx.provider_local_state,
        ctx.local_storage,
        ctx.relay_c_port,
        relay_c_identity.public_key,
        :local
      )

    on_exit(fn -> stop_and_assert_child(provider_local) end)
    on_exit(fn -> stop_and_assert_child(provider_direct) end)
    on_exit(fn -> stop_and_assert_child(provider_network) end)

    assert_ready(provider_network.port, "Serving Private Files")
    assert_ready(provider_direct.port, "Serving Private Files")
    assert_ready(provider_local.port, "Serving Private Files")

    relay_a_url = "127.0.0.1:#{ctx.relay_a_port}"
    relay_a_args = relay_args(ctx.relay_a_port, relay_a_identity.public_key)

    assert_eventually(fn ->
      case probe(ctx.citizen_a_state, relay_a_url, relay_a_identity.public_key, "files") do
        %{"cached" => true, "providers" => providers} ->
          provider_network_identity.public_key in providers and
            provider_direct_identity.public_key not in providers and
            provider_local_identity.public_key not in providers

        _ ->
          false
      end
    end)

    assert %{"cached" => true, "providers" => providers} =
             probe(ctx.citizen_a_state, relay_a_url, relay_a_identity.public_key, "files")

    assert provider_network_identity.public_key in providers
    assert provider_direct_identity.public_key not in providers
    assert provider_local_identity.public_key not in providers

    assert %{"cached" => true, "providers" => []} =
             probe(
               ctx.citizen_a_state,
               relay_a_url,
               relay_a_identity.public_key,
               "no-such-capability"
             )

    # The exact key can be resolved using the distant relay's catalog and then
    # used for a normal capability detail request.
    info =
      run_child(ctx.citizen_a_state, [
        "info",
        provider_network_identity.public_key | relay_a_args
      ])

    assert info.status == 0, info.output
    assert info.output =~ "Private Files"

    assert_control_entries(ctx.citizen_a_state, [citizen_a_identity.public_key])
    assert_empty_mailbox(ctx.citizen_a_state)
    assert_empty_mailbox(ctx.provider_network_state)
    refute plaintext_present?(ctx.network_storage, provider_network_identity.public_key)

    # Stopping the publisher emits a catalog withdrawal. A continues to query
    # only its own relay, which reports the already-synchronised local catalog.
    assert :ok = stop_child(provider_network.port)
    assert_os_pid_stopped(provider_network.os_pid)

    assert_eventually(fn ->
      case probe(ctx.citizen_a_state, relay_a_url, relay_a_identity.public_key, "files") do
        %{"cached" => true, "providers" => providers} ->
          provider_network_identity.public_key not in providers

        _ ->
          false
      end
    end)

    # Restart the far partner with the same persistent identity. Its new epoch
    # must not resurrect the withdrawn provider from an old catalog snapshot.
    assert :ok = stop_child(relay_c.port)
    assert_os_pid_stopped(relay_c.os_pid)

    relay_c_restarted =
      start_relay(ctx.relay_c_state, relay_c_identity, ctx.relay_c_port,
        peers: [{relay_b_identity, ctx.relay_b_port}]
      )

    on_exit(fn -> stop_and_assert_child(relay_c_restarted) end)
    assert_ready(relay_c_restarted.port, "arc relay listening on port #{ctx.relay_c_port}")

    assert_eventually(fn ->
      case probe(ctx.citizen_a_state, relay_a_url, relay_a_identity.public_key, "files") do
        %{"cached" => true, "providers" => providers} ->
          provider_network_identity.public_key not in providers

        _ ->
          false
      end
    end)

    assert :ok = stop_child(provider_local.port)
    assert :ok = stop_child(provider_direct.port)
    assert :ok = stop_child(relay_c_restarted.port)
    assert :ok = stop_child(relay_b.port)
    assert :ok = stop_child(relay_a.port)
    assert_os_pid_stopped(provider_local.os_pid)
    assert_os_pid_stopped(provider_direct.os_pid)
    assert_os_pid_stopped(relay_c_restarted.os_pid)
    assert_os_pid_stopped(relay_b.os_pid)
    assert_os_pid_stopped(relay_a.os_pid)
  end

  test "a catalog withdrawal crosses a cycle without resurrecting", ctx do
    relay_a_identity = create_identity(ctx.relay_a_state)
    relay_b_identity = create_identity(ctx.relay_b_state)
    relay_c_identity = create_identity(ctx.relay_c_state)
    relay_d_identity = create_identity(ctx.relay_d_state)
    _citizen_a_identity = create_identity(ctx.citizen_a_state)
    provider_network_identity = create_identity(ctx.provider_network_state)

    relay_a =
      start_relay(ctx.relay_a_state, relay_a_identity, ctx.relay_a_port,
        peers: [{relay_b_identity, ctx.relay_b_port}]
      )

    relay_b =
      start_relay(ctx.relay_b_state, relay_b_identity, ctx.relay_b_port,
        peers: [
          {relay_a_identity, ctx.relay_a_port},
          {relay_c_identity, ctx.relay_c_port},
          {relay_d_identity, ctx.relay_d_port}
        ],
        transit?: true
      )

    relay_c =
      start_relay(ctx.relay_c_state, relay_c_identity, ctx.relay_c_port,
        peers: [{relay_b_identity, ctx.relay_b_port}, {relay_d_identity, ctx.relay_d_port}],
        transit?: true
      )

    relay_d =
      start_relay(ctx.relay_d_state, relay_d_identity, ctx.relay_d_port,
        peers: [{relay_b_identity, ctx.relay_b_port}, {relay_c_identity, ctx.relay_c_port}]
      )

    on_exit(fn -> stop_and_assert_child(relay_d) end)
    on_exit(fn -> stop_and_assert_child(relay_c) end)
    on_exit(fn -> stop_and_assert_child(relay_b) end)
    on_exit(fn -> stop_and_assert_child(relay_a) end)

    assert_ready(relay_a.port, "arc relay listening on port #{ctx.relay_a_port}")
    assert_ready(relay_b.port, "arc relay listening on port #{ctx.relay_b_port}")
    assert_ready(relay_c.port, "arc relay listening on port #{ctx.relay_c_port}")
    assert_ready(relay_d.port, "arc relay listening on port #{ctx.relay_d_port}")

    provider_network =
      start_provider(
        ctx.provider_network_state,
        ctx.network_storage,
        ctx.relay_d_port,
        relay_d_identity.public_key,
        :network
      )

    on_exit(fn -> stop_and_assert_child(provider_network) end)
    assert_ready(provider_network.port, "Serving Private Files")

    relay_a_url = "127.0.0.1:#{ctx.relay_a_port}"

    assert_eventually(fn ->
      case probe(ctx.citizen_a_state, relay_a_url, relay_a_identity.public_key, "files") do
        %{"cached" => true, "providers" => providers} ->
          provider_network_identity.public_key in providers

        _ ->
          false
      end
    end)

    assert :ok = stop_child(provider_network.port)
    assert_os_pid_stopped(provider_network.os_pid)

    assert_eventually(fn ->
      case probe(ctx.citizen_a_state, relay_a_url, relay_a_identity.public_key, "files") do
        %{"cached" => true, "providers" => providers} ->
          provider_network_identity.public_key not in providers

        _ ->
          false
      end
    end)

    assert %{"cached" => true, "providers" => providers} =
             probe(ctx.citizen_a_state, relay_a_url, relay_a_identity.public_key, "files")

    assert provider_network_identity.public_key not in providers
    assert_control_entries(ctx.citizen_a_state, [])
    assert_empty_mailbox(ctx.citizen_a_state)
    assert_empty_mailbox(ctx.provider_network_state)

    assert :ok = stop_child(relay_d.port)
    assert :ok = stop_child(relay_c.port)
    assert :ok = stop_child(relay_b.port)
    assert :ok = stop_child(relay_a.port)
    assert_os_pid_stopped(relay_d.os_pid)
    assert_os_pid_stopped(relay_c.os_pid)
    assert_os_pid_stopped(relay_b.os_pid)
    assert_os_pid_stopped(relay_a.os_pid)
  end

  defp create_identity(state_root) do
    result = run_child(state_root, ["keys", "gen"])
    assert result.status == 0, result.output

    [_, name] = Regex.run(~r/^\s*name:\s+(\S+)$/m, result.output)
    [_, public_key] = Regex.run(~r/^\s*public_key:\s+([a-f0-9]{64})$/m, result.output)
    %{name: name, public_key: public_key}
  end

  defp start_relay(state_root, identity, port, opts) do
    peers =
      opts
      |> Keyword.fetch!(:peers)
      |> Enum.flat_map(fn {peer, peer_port} ->
        ["--peer", "#{peer.public_key}@127.0.0.1:#{peer_port}"]
      end)

    args = ["relay", "--port", Integer.to_string(port), "--key", identity.name] ++ peers
    args = if opts[:transit?], do: args ++ ["--transit"], else: args
    child = open_child(@child_script, state_root, args)
    {:os_pid, os_pid} = Port.info(child, :os_pid)
    %{port: child, os_pid: os_pid}
  end

  defp start_provider(state_root, storage_root, relay_port, relay_public_key, scope) do
    args = [
      "serve",
      @provider_bundle,
      "--relay",
      "127.0.0.1:#{relay_port}",
      "--relay-pubkey",
      relay_public_key
    ]

    args =
      case scope do
        :network -> args ++ ["--federate-network"]
        :direct -> args ++ ["--federate"]
        :local -> args
      end

    child =
      open_child(@child_script, state_root, args, [
        {"FILES_ROOT", storage_root},
        {"FILES_QUOTA_BYTES", "16777216"}
      ])

    {:os_pid, os_pid} = Port.info(child, :os_pid)
    %{port: child, os_pid: os_pid}
  end

  defp probe(state_root, relay, relay_public_key, query) do
    port = open_child(@probe_script, state_root, [relay, relay_public_key, query])
    result = await_child(port, System.monotonic_time(:millisecond) + @timeout, "")

    if result.status == 0 do
      :json.decode(String.trim(result.output))
    else
      %{"error" => result.output}
    end
  rescue
    _ -> %{"error" => "invalid_probe_reply"}
  end

  defp assert_ready(port, expected) do
    deadline = System.monotonic_time(:millisecond) + @timeout
    await_ready(port, expected, deadline, "")
  end

  defp await_ready(port, expected, deadline, output) do
    remaining = deadline - System.monotonic_time(:millisecond)
    assert remaining > 0, "child did not become ready:\n#{output}"

    receive do
      {^port, {:data, chunk}} ->
        output = output <> chunk

        if String.contains?(output, expected),
          do: :ok,
          else: await_ready(port, expected, deadline, output)

      {^port, {:exit_status, status}} ->
        flunk("child exited before readiness (#{status}):\n#{output}")
    after
      min(remaining, 250) -> await_ready(port, expected, deadline, output)
    end
  end

  defp run_child(state_root, argv) do
    port = open_child(@child_script, state_root, argv)
    await_child(port, System.monotonic_time(:millisecond) + @timeout, "")
  end

  defp open_child(script, state_root, argv, extra \\ []) do
    mix = System.find_executable("mix") || raise "mix executable is unavailable"
    args = ["run", "--no-compile", "--no-start", script, "--", state_root | argv]

    Port.open(
      {:spawn_executable, String.to_charlist(mix)},
      [
        :binary,
        :exit_status,
        :use_stdio,
        :stderr_to_stdout,
        args: Enum.map(args, &String.to_charlist/1),
        cd: String.to_charlist(@project_root),
        env:
          state_root
          |> child_env(extra)
          |> Enum.map(fn {key, value} -> {String.to_charlist(key), String.to_charlist(value)} end)
      ]
    )
  end

  defp await_child(port, deadline, output) do
    remaining = deadline - System.monotonic_time(:millisecond)

    if remaining <= 0 do
      stop_child(port)
      flunk("child command exceeded #{@timeout}ms:\n#{output}")
    end

    receive do
      {^port, {:data, chunk}} -> await_child(port, deadline, output <> chunk)
      {^port, {:exit_status, status}} -> %{output: output, status: status}
    after
      min(remaining, 250) -> await_child(port, deadline, output)
    end
  end

  defp relay_args(port, public_key),
    do: ["--relay", "127.0.0.1:#{port}", "--relay-pubkey", public_key]

  defp child_env(state_root, extra) do
    [
      {"MIX_ENV", "test"},
      {"ARC_RELAY_MAX_FRAME_BYTES", "6291456"},
      {"ARC_TEST_STATE", state_root}
      | Enum.to_list(extra)
    ]
  end

  defp assert_eventually(check, timeout \\ @timeout) do
    deadline = System.monotonic_time(:millisecond) + timeout
    await_eventually(check, deadline)
  end

  defp await_eventually(check, deadline) do
    if check.() do
      :ok
    else
      remaining = deadline - System.monotonic_time(:millisecond)

      assert remaining > 0, "condition did not become true before timeout"

      receive do
      after
        min(remaining, 200) -> await_eventually(check, deadline)
      end
    end
  end

  defp assert_control_entries(state_root, expected_keys) do
    actual_keys =
      state_root
      |> Path.join("control/*.entry")
      |> Path.wildcard()
      |> Enum.map(&(&1 |> Path.basename() |> String.replace_suffix(".entry", "")))
      |> Enum.sort()

    assert actual_keys == Enum.sort(expected_keys)
  end

  defp assert_empty_mailbox(state_root),
    do: assert(state_root |> Path.join("mailbox/**/*") |> Path.wildcard() == [])

  defp plaintext_present?(root, value) do
    root
    |> Path.join("**/*")
    |> Path.wildcard()
    |> Enum.any?(fn path ->
      :binary.match(path, value) != :nomatch or
        (File.regular?(path) and :binary.match(File.read!(path), value) != :nomatch)
    end)
  end

  defp stop_child(port) when is_port(port) do
    # An on_exit callback does not own the port and receives none of its
    # messages. A port monitor reports the close to any process.
    ref = Port.monitor(port)

    try do
      Port.command(port, "shutdown\n")
    rescue
      ArgumentError -> :ok
    end

    await_shutdown(port, ref, System.monotonic_time(:millisecond) + 2_000, "")
  end

  defp await_shutdown(port, ref, deadline, output) do
    remaining = deadline - System.monotonic_time(:millisecond)
    if remaining <= 0, do: shutdown_timeout(port, output)

    receive do
      {^port, {:data, chunk}} ->
        await_shutdown(port, ref, deadline, output <> chunk)

      {^port, {:exit_status, 0}} ->
        Process.demonitor(ref, [:flush])
        :ok

      {^port, {:exit_status, status}} ->
        flunk("child exited with status #{status}:\n#{output}")

      {:DOWN, ^ref, :port, ^port, _reason} ->
        :ok
    after
      remaining ->
        shutdown_timeout(port, output)
    end
  end

  defp shutdown_timeout(port, output) do
    if Port.info(port), do: Port.close(port)
    flunk("child ignored shutdown request:\n#{output}")
  end

  defp stop_and_assert_child(%{port: port, os_pid: os_pid}) do
    assert :ok = stop_child(port)
    assert_os_pid_stopped(os_pid)
  end

  defp assert_os_pid_stopped(os_pid) do
    deadline = System.monotonic_time(:millisecond) + 2_000
    await_os_pid_stopped(os_pid, deadline)
  end

  defp await_os_pid_stopped(os_pid, deadline) do
    {output, _status} = System.cmd("/bin/ps", ["-p", Integer.to_string(os_pid), "-o", "pid="])

    if String.trim(output) == "" do
      :ok
    else
      remaining = deadline - System.monotonic_time(:millisecond)
      assert remaining > 0, "child process #{os_pid} still running"

      receive do
      after
        min(remaining, 100) -> await_os_pid_stopped(os_pid, deadline)
      end
    end
  end

  defp unused_tcp_port do
    {:ok, socket} = :gen_tcp.listen(0, [:binary, active: false, reuseaddr: true])
    {:ok, port} = :inet.port(socket)
    :ok = :gen_tcp.close(socket)
    port
  end
end
