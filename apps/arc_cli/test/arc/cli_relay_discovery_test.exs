defmodule Arc.CLI.RelayDiscoveryIntegrationTest do
  use ExUnit.Case, async: false

  alias Arc.Identity

  @project_root Path.expand("../../../..", __DIR__)
  @child_script Path.join(@project_root, "test/fixtures/relay_discovery/arc_child.exs")
  @provider_bundle Path.expand("../../../../providers/files", __DIR__)
  @timeout 20_000

  setup do
    root =
      Path.join(System.tmp_dir!(), "arc-relay-discovery-#{System.unique_integer([:positive])}")

    provider_state = Path.join(root, "provider")
    citizen_state = Path.join(root, "citizen")
    storage_root = Path.join(root, "storage")
    source = Path.join(root, "source.bin")
    output = Path.join(root, "restored.bin")

    File.mkdir_p!(root)
    bytes = <<0, 255, 128, 10, 13>> <> "relay-private-#{System.unique_integer([:positive])}"
    File.write!(source, bytes)

    relay_key = Identity.generate().public_key
    {:ok, relay} = Arc.Net.Relay.start_link(0, relay_public_key: relay_key)
    relay_port = Arc.Net.Relay.get_port(relay)
    relay_pin = Base.encode16(relay_key, case: :lower)
    telemetry_id = "relay-discovery-#{System.unique_integer([:positive])}"

    :ok =
      :telemetry.attach_many(
        telemetry_id,
        [[:arc, :net, :relay, :packet, :forwarded]],
        &__MODULE__.handle_telemetry/4,
        self()
      )

    on_exit(fn ->
      :telemetry.detach(telemetry_id)

      if Process.alive?(relay) do
        GenServer.stop(relay, :normal)
      end

      File.rm_rf!(root)
    end)

    %{
      root: root,
      bytes: bytes,
      source: source,
      output: output,
      provider_state: provider_state,
      citizen_state: citizen_state,
      storage_root: storage_root,
      relay_port: relay_port,
      relay_pin: relay_pin
    }
  end

  test "separate citizens discover and use a private files provider only through a relay", ctx do
    provider_identity = create_identity(ctx.provider_state)
    citizen = create_identity(ctx.citizen_state)
    provider = start_provider(ctx, provider_identity)

    on_exit(fn -> stop_child(provider.port) end)

    assert_provider_ready(provider.port, ctx.relay_port)

    # These directories are deliberately distinct. A discovery must be a relay
    # query, not an entry copied into the citizen's file-backed control plane.
    assert_control_entries(ctx.provider_state, [provider_identity.public_key])
    assert_control_entries(ctx.citizen_state, [])

    relay_args = relay_args(ctx)

    discover = run_child(ctx.citizen_state, ["discover", "files" | relay_args])
    assert discover.status == 0, discover.output
    assert discover.output =~ provider_identity.name
    assert discover.output =~ "files"

    info = run_child(ctx.citizen_state, ["info", provider_identity.name | relay_args])
    assert info.status == 0, info.output
    assert info.output =~ "Private Files"

    install =
      run_child(ctx.citizen_state, [
        "install",
        provider_identity.name,
        "primary",
        "--trust" | relay_args
      ])

    assert install.status == 0, install.output
    assert install.output =~ "Installed files"

    put = run_child(ctx.citizen_state, ["files", "put", ctx.source | relay_args])
    assert put.status == 0, put.output
    assert [file_id] = Regex.run(~r/\b[a-f0-9]{64}\b/, put.output)

    list = run_child(ctx.citizen_state, ["files", "list" | relay_args])
    assert list.status == 0, list.output
    assert list.output =~ file_id
    assert list.output =~ Path.basename(ctx.source)

    get =
      run_child(ctx.citizen_state, ["files", "get", file_id, "--output", ctx.output | relay_args])

    assert get.status == 0, get.output
    assert File.read!(ctx.output) == ctx.bytes

    assert_control_entries(ctx.provider_state, [provider_identity.public_key])
    assert_control_entries(ctx.citizen_state, [citizen.public_key])
    assert_empty_mailbox(ctx.provider_state)
    assert_empty_mailbox(ctx.citizen_state)
    refute plaintext_present?(ctx.storage_root, Path.basename(ctx.source))
    refute plaintext_present?(ctx.storage_root, ctx.bytes)
    assert relay_forward_count() >= 8
    assert :ok = stop_child(provider.port)
    assert_os_pid_stopped(provider.os_pid)
  end

  def handle_telemetry(_event, _measurements, _metadata, test_pid) do
    send(test_pid, :relay_packet_forwarded)
  end

  defp create_identity(state_root) do
    result = run_child(state_root, ["keys", "gen"])
    assert result.status == 0, result.output

    [_, name] = Regex.run(~r/^\s*name:\s+(\S+)$/m, result.output)
    [_, public_key] = Regex.run(~r/^\s*public_key:\s+([a-f0-9]{64})$/m, result.output)
    %{name: name, public_key: public_key}
  end

  defp start_provider(ctx, provider_identity) do
    args = [
      "serve",
      @provider_bundle,
      "--relay",
      "127.0.0.1:#{ctx.relay_port}",
      "--relay-pubkey",
      ctx.relay_pin
    ]

    port =
      open_child(ctx.provider_state, args, [
        {"FILES_ROOT", ctx.storage_root},
        {"FILES_QUOTA_BYTES", "16777216"}
      ])

    {:os_pid, os_pid} = Port.info(port, :os_pid)
    %{port: port, os_pid: os_pid, identity: provider_identity}
  end

  defp assert_provider_ready(port, relay_port) do
    deadline = System.monotonic_time(:millisecond) + @timeout
    await_provider_ready(port, relay_port, deadline, "")
  end

  defp await_provider_ready(port, relay_port, deadline, output) do
    remaining = deadline - System.monotonic_time(:millisecond)
    assert remaining > 0, "provider did not become ready:\n#{output}"

    receive do
      {^port, {:data, chunk}} ->
        output = output <> chunk

        if output =~ "Serving Private Files" and output =~ "Relay: 127.0.0.1:#{relay_port}" do
          :ok
        else
          await_provider_ready(port, relay_port, deadline, output)
        end

      {^port, {:exit_status, status}} ->
        flunk("provider exited before readiness (#{status}):\n#{output}")
    after
      min(remaining, 250) -> await_provider_ready(port, relay_port, deadline, output)
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

  defp relay_args(ctx) do
    ["--relay", "127.0.0.1:#{ctx.relay_port}", "--relay-pubkey", ctx.relay_pin]
  end

  defp child_env(state_root, extra) do
    [
      {"MIX_ENV", "test"},
      {"ARC_RELAY_MAX_FRAME_BYTES", "6291456"},
      {"ARC_TEST_STATE", state_root}
      | Enum.to_list(extra)
    ]
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

  defp assert_empty_mailbox(state_root) do
    assert state_root |> Path.join("mailbox/**/*") |> Path.wildcard() == []
  end

  defp plaintext_present?(root, value) do
    root
    |> Path.join("**/*")
    |> Path.wildcard()
    |> Enum.any?(fn path ->
      :binary.match(path, value) != :nomatch or
        (File.regular?(path) and :binary.match(File.read!(path), value) != :nomatch)
    end)
  end

  defp relay_forward_count(count \\ 0) do
    receive do
      :relay_packet_forwarded -> relay_forward_count(count + 1)
    after
      100 -> count
    end
  end

  defp stop_child(port) when is_port(port) do
    if Port.info(port) do
      true = Port.command(port, "shutdown\n")

      receive do
        {^port, {:data, _chunk}} -> stop_child(port)
        {^port, {:exit_status, 0}} -> :ok
        {^port, {:exit_status, status}} -> flunk("child exited with status #{status}")
      after
        2_000 ->
          Port.close(port)
          flunk("child ignored shutdown request")
      end
    else
      :ok
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
      assert remaining > 0, "provider process #{os_pid} still running"

      receive do
      after
        min(remaining, 100) -> await_os_pid_stopped(os_pid, deadline)
      end
    end
  end
end
