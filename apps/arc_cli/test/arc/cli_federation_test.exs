defmodule Arc.CLI.FederationIntegrationTest do
  use ExUnit.Case, async: false

  @project_root Path.expand("../../../..", __DIR__)
  @child_script Path.join(@project_root, "test/fixtures/federation/arc_child.exs")
  @provider_bundle Path.expand("../../../../providers/files", __DIR__)
  @timeout 20_000

  setup do
    root = Path.join(System.tmp_dir!(), "arc-federation-#{System.unique_integer([:positive])}")
    File.mkdir_p!(root)

    source = Path.join(root, "federated-private.bin")
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
      provider_federated_state: Path.join(root, "provider-federated"),
      provider_local_state: Path.join(root, "provider-local"),
      citizen_a_state: Path.join(root, "citizen-a"),
      citizen_b_state: Path.join(root, "citizen-b"),
      federated_storage: Path.join(root, "federated-storage"),
      local_storage: Path.join(root, "local-storage"),
      relay_a_port: unused_tcp_port(),
      relay_b_port: unused_tcp_port()
    }
  end

  test "directly federated relays expose only opted-in providers and carry private file traffic",
       ctx do
    relay_a_identity = create_identity(ctx.relay_a_state)
    relay_b_identity = create_identity(ctx.relay_b_state)
    provider_federated_identity = create_identity(ctx.provider_federated_state)
    provider_local_identity = create_identity(ctx.provider_local_state)
    citizen_a_identity = create_identity(ctx.citizen_a_state)
    citizen_b_identity = create_identity(ctx.citizen_b_state)

    relay_a =
      start_relay(
        ctx.relay_a_state,
        relay_a_identity,
        ctx.relay_a_port,
        relay_b_identity,
        ctx.relay_b_port
      )

    relay_b =
      start_relay(
        ctx.relay_b_state,
        relay_b_identity,
        ctx.relay_b_port,
        relay_a_identity,
        ctx.relay_a_port
      )

    assert_ready(relay_a.port, "arc relay listening on port #{ctx.relay_a_port}")
    assert_ready(relay_b.port, "arc relay listening on port #{ctx.relay_b_port}")

    provider_federated =
      start_provider(
        ctx.provider_federated_state,
        ctx.federated_storage,
        ctx.relay_b_port,
        relay_b_identity.public_key,
        federate?: true
      )

    provider_local =
      start_provider(
        ctx.provider_local_state,
        ctx.local_storage,
        ctx.relay_b_port,
        relay_b_identity.public_key,
        federate?: false
      )

    on_exit(fn -> stop_child(provider_local.port) end)
    on_exit(fn -> stop_child(provider_federated.port) end)
    on_exit(fn -> stop_child(relay_b.port) end)
    on_exit(fn -> stop_child(relay_a.port) end)

    assert_ready(provider_federated.port, "Serving Private Files")
    assert_ready(provider_local.port, "Serving Private Files")

    relay_a_args = relay_args(ctx.relay_a_port, relay_a_identity.public_key)
    relay_b_args = relay_args(ctx.relay_b_port, relay_b_identity.public_key)

    # A local member of B sees both providers. A citizen of A sees the offer
    # that the provider explicitly allowed to cross the direct federation only.
    assert_eventually(fn ->
      result = run_child(ctx.citizen_b_state, ["discover", "files" | relay_b_args])

      result.status == 0 and result.output =~ provider_federated_identity.name and
        result.output =~ provider_local_identity.name
    end)

    assert_eventually(fn ->
      result = run_child(ctx.citizen_a_state, ["discover", "files" | relay_a_args])

      result.status == 0 and result.output =~ provider_federated_identity.name and
        result.output =~ "files" and result.output =~ "Matches on this relay page" and
        not String.contains?(result.output, provider_local_identity.name)
    end)

    info =
      run_child(ctx.citizen_a_state, ["info", provider_federated_identity.name | relay_a_args])

    assert info.status == 0, info.output
    assert info.output =~ "Private Files"

    install =
      run_child(ctx.citizen_a_state, [
        "install",
        provider_federated_identity.name,
        "primary",
        "--trust" | relay_a_args
      ])

    assert install.status == 0, install.output
    assert install.output =~ "Installed files"

    put = run_child(ctx.citizen_a_state, ["files", "put", ctx.source | relay_a_args])
    assert put.status == 0, put.output
    assert [file_id] = Regex.run(~r/\b[a-f0-9]{64}\b/, put.output)

    list = run_child(ctx.citizen_a_state, ["files", "list" | relay_a_args])
    assert list.status == 0, list.output
    assert list.output =~ file_id
    assert list.output =~ Path.basename(ctx.source)

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

    assert_control_entries(ctx.provider_federated_state, [provider_federated_identity.public_key])
    assert_control_entries(ctx.provider_local_state, [provider_local_identity.public_key])
    assert_control_entries(ctx.citizen_a_state, [citizen_a_identity.public_key])
    assert_control_entries(ctx.citizen_b_state, [citizen_b_identity.public_key])
    assert_empty_mailbox(ctx.provider_federated_state)
    assert_empty_mailbox(ctx.provider_local_state)
    assert_empty_mailbox(ctx.citizen_a_state)
    assert_empty_mailbox(ctx.citizen_b_state)
    refute plaintext_present?(ctx.federated_storage, Path.basename(ctx.source))
    refute plaintext_present?(ctx.federated_storage, ctx.bytes)

    assert :ok = stop_child(relay_b.port)
    assert_os_pid_stopped(relay_b.os_pid)

    assert_eventually(fn ->
      result = run_child(ctx.citizen_a_state, ["discover", "files" | relay_a_args])
      result.status == 0 and not String.contains?(result.output, provider_federated_identity.name)
    end)

    assert :ok = stop_child(provider_local.port)
    assert :ok = stop_child(provider_federated.port)
    assert :ok = stop_child(relay_a.port)
    assert_os_pid_stopped(provider_local.os_pid)
    assert_os_pid_stopped(provider_federated.os_pid)
    assert_os_pid_stopped(relay_a.os_pid)
  end

  defp create_identity(state_root) do
    result = run_child(state_root, ["keys", "gen"])
    assert result.status == 0, result.output

    [_, name] = Regex.run(~r/^\s*name:\s+(\S+)$/m, result.output)
    [_, public_key] = Regex.run(~r/^\s*public_key:\s+([a-f0-9]{64})$/m, result.output)
    %{name: name, public_key: public_key}
  end

  defp start_relay(state_root, identity, port, peer, peer_port) do
    child =
      open_child(state_root, [
        "relay",
        "--port",
        Integer.to_string(port),
        "--key",
        identity.name,
        "--peer",
        "#{peer.public_key}@127.0.0.1:#{peer_port}"
      ])

    {:os_pid, os_pid} = Port.info(child, :os_pid)
    %{port: child, os_pid: os_pid}
  end

  defp start_provider(state_root, storage_root, relay_port, relay_public_key, opts) do
    args = [
      "serve",
      @provider_bundle,
      "--relay",
      "127.0.0.1:#{relay_port}",
      "--relay-pubkey",
      relay_public_key
    ]

    args = if opts[:federate?], do: args ++ ["--federate"], else: args

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
