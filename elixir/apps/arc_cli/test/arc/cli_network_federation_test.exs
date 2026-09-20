defmodule Arc.CLI.NetworkFederationIntegrationTest do
  use ExUnit.Case, async: false

  @project_root Path.expand("../../../..", __DIR__)
  @child_script Path.join(@project_root, "test/fixtures/federation/arc_child.exs")
  @provider_bundle Path.expand("../../../../providers/files", __DIR__)
  @timeout 30_000

  setup do
    root =
      Path.join(System.tmp_dir!(), "arc-network-federation-#{System.unique_integer([:positive])}")

    File.mkdir_p!(root)

    source = Path.join(root, "network-private.bin")
    output = Path.join(root, "restored.bin")
    bytes = <<0, 255, 128, 10, 13>> <> :crypto.strong_rand_bytes(512 * 1024)
    File.write!(source, bytes)

    on_exit(fn -> File.rm_rf!(root) end)

    %{
      root: root,
      source: source,
      output: output,
      bytes: bytes,
      relay_a_state: Path.join(root, "relay-a"),
      relay_b_state: Path.join(root, "relay-b"),
      relay_c_state: Path.join(root, "relay-c"),
      relay_d_state: Path.join(root, "relay-d"),
      citizen_a_state: Path.join(root, "citizen-a"),
      citizen_c_state: Path.join(root, "citizen-c"),
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

  test "network offers travel through two transit relays and stop when transit is removed", ctx do
    relay_a_identity = create_identity(ctx.relay_a_state)
    relay_b_identity = create_identity(ctx.relay_b_state)
    relay_c_identity = create_identity(ctx.relay_c_state)
    relay_d_identity = create_identity(ctx.relay_d_state)
    citizen_a_identity = create_identity(ctx.citizen_a_state)
    citizen_c_identity = create_identity(ctx.citizen_c_state)
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
        peers: [{relay_b_identity, ctx.relay_b_port}, {relay_d_identity, ctx.relay_d_port}],
        transit?: true
      )

    relay_d =
      start_relay(ctx.relay_d_state, relay_d_identity, ctx.relay_d_port,
        peers: [{relay_c_identity, ctx.relay_c_port}]
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

    provider_direct =
      start_provider(
        ctx.provider_direct_state,
        ctx.direct_storage,
        ctx.relay_d_port,
        relay_d_identity.public_key,
        :direct
      )

    provider_local =
      start_provider(
        ctx.provider_local_state,
        ctx.local_storage,
        ctx.relay_d_port,
        relay_d_identity.public_key,
        :local
      )

    on_exit(fn -> stop_and_assert_child(provider_local) end)
    on_exit(fn -> stop_and_assert_child(provider_direct) end)
    on_exit(fn -> stop_and_assert_child(provider_network) end)

    assert_ready(provider_network.port, "Serving Private Files")
    assert_ready(provider_direct.port, "Serving Private Files")
    assert_ready(provider_local.port, "Serving Private Files")

    relay_a_args = relay_args(ctx.relay_a_port, relay_a_identity.public_key)
    relay_c_args = relay_args(ctx.relay_c_port, relay_c_identity.public_key)

    # C is D's direct neighbour. It can see D's direct offer but never its
    # local-only offer. A is two relay hops farther away and sees only v3.
    assert_eventually(fn ->
      result = run_child(ctx.citizen_c_state, ["discover", "files" | relay_c_args])

      result.status == 0 and result.output =~ provider_network_identity.name and
        result.output =~ provider_direct_identity.name and
        not String.contains?(result.output, provider_local_identity.name)
    end)

    assert_eventually(fn ->
      result = run_child(ctx.citizen_a_state, ["discover", "files" | relay_a_args])

      result.status == 0 and result.output =~ provider_network_identity.name and
        not String.contains?(result.output, provider_direct_identity.name) and
        not String.contains?(result.output, provider_local_identity.name)
    end)

    info = run_child(ctx.citizen_a_state, ["info", provider_network_identity.name | relay_a_args])
    assert info.status == 0, info.output
    assert info.output =~ "Private Files"

    install =
      run_child(ctx.citizen_a_state, [
        "install",
        provider_network_identity.name,
        "primary",
        "--trust" | relay_a_args
      ])

    assert install.status == 0, install.output

    put = run_child(ctx.citizen_a_state, ["files", "put", ctx.source | relay_a_args])
    assert put.status == 0, put.output
    assert [file_id] = Regex.run(~r/\b[a-f0-9]{64}\b/, put.output)

    get =
      run_child(ctx.citizen_a_state, [
        "files",
        "get",
        file_id,
        "--output",
        ctx.output | relay_a_args
      ])

    assert get.status == 0, get.output
    assert File.read!(ctx.output) == ctx.bytes

    assert_control_entries(ctx.citizen_a_state, [citizen_a_identity.public_key])
    assert_control_entries(ctx.citizen_c_state, [citizen_c_identity.public_key])
    assert_control_entries(ctx.provider_network_state, [provider_network_identity.public_key])
    assert_control_entries(ctx.provider_direct_state, [provider_direct_identity.public_key])
    assert_control_entries(ctx.provider_local_state, [provider_local_identity.public_key])
    assert_empty_mailbox(ctx.citizen_a_state)
    assert_empty_mailbox(ctx.citizen_c_state)
    assert_empty_mailbox(ctx.provider_network_state)
    assert_empty_mailbox(ctx.provider_direct_state)
    assert_empty_mailbox(ctx.provider_local_state)
    refute plaintext_present?(ctx.network_storage, Path.basename(ctx.source))
    refute plaintext_present?(ctx.network_storage, ctx.bytes)

    # Restart B with the same persistent identity and adjacent peers, but
    # remove its operator's transit consent. A must lose D's network offer and
    # its already installed command must not route through any fallback.
    assert :ok = stop_child(relay_b.port)
    assert_os_pid_stopped(relay_b.os_pid)

    relay_b_without_transit =
      start_relay(ctx.relay_b_state, relay_b_identity, ctx.relay_b_port,
        peers: [{relay_a_identity, ctx.relay_a_port}, {relay_c_identity, ctx.relay_c_port}]
      )

    on_exit(fn -> stop_and_assert_child(relay_b_without_transit) end)
    assert_ready(relay_b_without_transit.port, "arc relay listening on port #{ctx.relay_b_port}")

    assert_eventually(fn ->
      result = run_child(ctx.citizen_a_state, ["discover", "files" | relay_a_args])
      result.status == 0 and not String.contains?(result.output, provider_network_identity.name)
    end)

    list_after_transit_removed = run_child(ctx.citizen_a_state, ["files", "list" | relay_a_args])
    assert list_after_transit_removed.status != 0, list_after_transit_removed.output

    assert :ok = stop_child(provider_local.port)
    assert :ok = stop_child(provider_direct.port)
    assert :ok = stop_child(provider_network.port)
    assert :ok = stop_child(relay_d.port)
    assert :ok = stop_child(relay_c.port)
    assert :ok = stop_child(relay_b_without_transit.port)
    assert :ok = stop_child(relay_a.port)
    assert_os_pid_stopped(provider_local.os_pid)
    assert_os_pid_stopped(provider_direct.os_pid)
    assert_os_pid_stopped(provider_network.os_pid)
    assert_os_pid_stopped(relay_d.os_pid)
    assert_os_pid_stopped(relay_c.os_pid)
    assert_os_pid_stopped(relay_b_without_transit.os_pid)
    assert_os_pid_stopped(relay_a.os_pid)
  end

  test "cyclic transit topology returns a network offer once without looping", ctx do
    relay_a_identity = create_identity(ctx.relay_a_state)
    relay_b_identity = create_identity(ctx.relay_b_state)
    relay_c_identity = create_identity(ctx.relay_c_state)
    relay_d_identity = create_identity(ctx.relay_d_state)
    citizen_a_identity = create_identity(ctx.citizen_a_state)
    provider_network_identity = create_identity(ctx.provider_network_state)

    # B-C-D-B is a cycle. A is attached only to B, so reaching D still
    # requires transit, while B's two paths to D exercise loop suppression and
    # duplicate-result reduction.
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

    relay_a_args = relay_args(ctx.relay_a_port, relay_a_identity.public_key)

    assert_eventually(fn ->
      result = run_child(ctx.citizen_a_state, ["discover", "files" | relay_a_args])

      result.status == 0 and
        String.contains?(
          result.output,
          "Expand: arc info #{provider_network_identity.public_key} primary"
        ) and
        occurrences(result.output, "\n#{provider_network_identity.name}/primary [data/files]\n") ==
          1
    end)

    # A cycle may exhaust a branch budget and mark search partial. The exact
    # public key is unambiguous, so it remains usable when this provider was
    # found on a successful branch.
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

    assert :ok = stop_child(provider_network.port)
    assert :ok = stop_child(relay_d.port)
    assert :ok = stop_child(relay_c.port)
    assert :ok = stop_child(relay_b.port)
    assert :ok = stop_child(relay_a.port)
    assert_os_pid_stopped(provider_network.os_pid)
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
    child = open_child(state_root, args)
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
      open_child(state_root, args, [
        {"FILES_ROOT", storage_root},
        {"FILES_QUOTA_BYTES", "16777216"}
      ])

    {:os_pid, os_pid} = Port.info(child, :os_pid)
    %{port: child, os_pid: os_pid}
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

        if String.contains?(output, expected) do
          :ok
        else
          await_ready(port, expected, deadline, output)
        end

      {^port, {:exit_status, status}} ->
        flunk("child exited before readiness (#{status}):\n#{output}")
    after
      min(remaining, 250) -> await_ready(port, expected, deadline, output)
    end
  end

  defp run_child(state_root, argv) do
    port = open_child(state_root, argv)
    await_child(port, System.monotonic_time(:millisecond) + @timeout, "")
  end

  defp open_child(state_root, argv, extra \\ []) do
    mix = System.find_executable("mix") || raise "mix executable is unavailable"
    args = ["run", "--no-compile", "--no-start", @child_script, "--", state_root | argv]

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

  defp occurrences(value, needle), do: length(String.split(value, needle)) - 1

  defp stop_child(port) when is_port(port) do
    # An on_exit callback does not own the port and receives none of its
    # messages. A port monitor reports the close to any process.
    ref = Port.monitor(port)

    try do
      Port.command(port, "shutdown\n")
    rescue
      ArgumentError -> :ok
    end

    await_stopped(port, ref)
  end

  defp await_stopped(port, ref) do
    receive do
      {^port, {:data, _chunk}} ->
        await_stopped(port, ref)

      {^port, {:exit_status, 0}} ->
        Process.demonitor(ref, [:flush])
        :ok

      {^port, {:exit_status, status}} ->
        flunk("child exited with status #{status}")

      {:DOWN, ^ref, :port, ^port, _reason} ->
        :ok
    after
      2_000 ->
        Port.close(port)
        flunk("child ignored shutdown request")
    end
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
