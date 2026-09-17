defmodule Arc.CLI.ProtocolIntegrationTest do
  use ExUnit.Case, async: false

  @project_root Path.expand("../../../..", __DIR__)
  @child_script Path.join(@project_root, "test/fixtures/protocol/arc_child.exs")
  @provider_bundle Path.expand("../../../../providers/sqlite", __DIR__)
  @binary_runtime Path.join(@project_root, "test/fixtures/providers/binary-echo-provider.exs")
  @binary_manifest Path.join(@project_root, "test/fixtures/providers/binary-echo-provider.json")
  @timeout 30_000

  setup do
    root = Path.join(System.tmp_dir!(), "arc-protocol-#{System.unique_integer([:positive])}")
    File.mkdir_p!(root)

    on_exit(fn -> File.rm_rf!(root) end)

    %{
      root: root,
      relay_a_state: Path.join(root, "relay-a"),
      relay_b_state: Path.join(root, "relay-b"),
      relay_c_state: Path.join(root, "relay-c"),
      provider_state: Path.join(root, "provider"),
      writer_state: Path.join(root, "writer"),
      reader_state: Path.join(root, "reader"),
      stranger_state: Path.join(root, "stranger"),
      relay_a_port: unused_tcp_port(),
      relay_b_port: unused_tcp_port(),
      relay_c_port: unused_tcp_port(),
      absent_relay_port: unused_tcp_port()
    }
  end

  test "sqlite request bytes travel through a transit relay network and preserve database grants",
       ctx do
    relay_a_identity = create_identity(ctx.relay_a_state)
    relay_b_identity = create_identity(ctx.relay_b_state)
    relay_c_identity = create_identity(ctx.relay_c_state)
    provider_identity = create_identity(ctx.provider_state)
    binary_provider_identity = create_identity(Path.join(ctx.root, "binary-provider"))
    writer = create_identity(ctx.writer_state)
    reader = create_identity(ctx.reader_state)
    _stranger = create_identity(ctx.stranger_state)

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

    for {relay, port} <- [
          {relay_a, ctx.relay_a_port},
          {relay_b, ctx.relay_b_port},
          {relay_c, ctx.relay_c_port}
        ] do
      assert_ready(relay.port, "arc relay listening on port #{port}")
    end

    database = Path.join(ctx.root, "main.sqlite3")
    config = Path.join(ctx.root, "sqlite-config.json")

    File.write!(
      config,
      :json.encode(%{
        "databases" => %{
          "main" => %{
            "path" => database,
            "grants" => %{writer.public_key => "write", reader.public_key => "read"}
          }
        }
      })
    )

    provider =
      start_provider(ctx.provider_state, config, ctx.relay_c_port, relay_c_identity.public_key)

    on_exit(fn -> stop_and_assert_child(provider) end)
    assert_ready(provider.port, "Serving SQLite over ARC")

    binary_provider =
      start_raw_provider(
        Path.join(ctx.root, "binary-provider"),
        ctx.relay_c_port,
        relay_c_identity.public_key
      )

    on_exit(fn -> stop_and_assert_child(binary_provider) end)
    assert_ready(binary_provider.port, "Serving Binary Echo")

    uri = "sqlite+arc://#{provider_identity.public_key}/main"
    relay_args = relay_args(ctx.relay_a_port, relay_a_identity.public_key)

    # The provider explicitly permits onward federation. The client reaches it
    # through A -> B (transit) -> C, without a control-plane record or a local
    # fallback route.
    create =
      run_child(ctx.writer_state, [
        "request",
        uri,
        "--body",
        ~s|{"sql":"CREATE TABLE notes (body TEXT)"}|
        | relay_args
      ])

    assert create.status == 0, create.output

    insert =
      run_child(ctx.writer_state, [
        "request",
        uri,
        "--body",
        ~s|{"sql":"INSERT INTO notes (body) VALUES (?)","params":["via relay"]}|
        | relay_args
      ])

    assert insert.status == 0, insert.output

    select =
      run_child(ctx.reader_state, [
        "request",
        uri,
        "--body",
        ~s|{"sql":"SELECT body FROM notes"}|
        | relay_args
      ])

    assert select.status == 0, select.output

    assert %{"results" => [%{"columns" => ["body"], "rows" => [["via relay"]]}]} =
             :json.decode(select.output)

    # A read-only identity cannot mutate the shared database, and an identity
    # absent from the provider's grant list cannot inspect it.
    denied_write =
      run_child(ctx.reader_state, [
        "request",
        uri,
        "--body",
        ~s|{"sql":"INSERT INTO notes (body) VALUES ('no')"}|
        | relay_args
      ])

    assert denied_write.status != 0
    assert denied_write.output =~ "write_denied"

    after_denied_write =
      run_child(ctx.reader_state, [
        "request",
        uri,
        "--body",
        ~s|{"sql":"SELECT body FROM notes"}|
        | relay_args
      ])

    assert after_denied_write.status == 0, after_denied_write.output

    assert %{"results" => [%{"rows" => [["via relay"]]}]} =
             :json.decode(after_denied_write.output)

    denied_read =
      run_child(ctx.stranger_state, [
        "request",
        uri,
        "--body",
        ~s|{"sql":"SELECT body FROM notes"}|
        | relay_args
      ])

    assert denied_read.status != 0
    assert denied_read.output =~ "unauthorized"

    wrong_scheme =
      run_child(ctx.reader_state, [
        "request",
        "postgres+arc://#{provider_identity.public_key}/main",
        "--body",
        ~s|{"sql":"SELECT body FROM notes"}|
        | relay_args
      ])

    assert wrong_scheme.status != 0

    wrong_path =
      run_child(ctx.reader_state, [
        "request",
        "sqlite+arc://#{provider_identity.public_key}/missing",
        "--body",
        ~s|{"sql":"SELECT body FROM notes"}|
        | relay_args
      ])

    assert wrong_path.status != 0
    assert Path.wildcard(Path.join(ctx.root, "**/*.sqlite3")) == [database]

    unavailable =
      run_child(ctx.writer_state, [
        "request",
        uri,
        "--body",
        ~s|{"sql":"SELECT body FROM notes"}|,
        "--timeout",
        "100"
        | relay_args(ctx.absent_relay_port, relay_a_identity.public_key)
      ])

    assert unavailable.status != 0
    refute unavailable.output =~ "via relay"

    # Generic protocol requests preserve opaque bytes. The input begins with
    # NUL and invalid UTF-8 and is larger than an exec runtime's line chunk.
    bytes = <<0, 255, 128, 10, 13>> <> :crypto.strong_rand_bytes(80 * 1024)
    input = Path.join(ctx.root, "binary-input")
    output = Path.join(ctx.root, "binary-output")
    File.write!(input, bytes)

    binary_uri = "binary-echo+arc://#{binary_provider_identity.public_key}/"

    stdout =
      run_child(ctx.writer_state, ["request", binary_uri, "--input", input | relay_args])

    assert stdout.status == 0
    assert stdout.output == bytes

    file_output =
      run_child(ctx.writer_state, [
        "request",
        binary_uri,
        "--input",
        input,
        "--output",
        output
        | relay_args
      ])

    assert file_output.status == 0, file_output.output

    assert file_output.output == ""
    assert File.read!(output) == bytes

    stdin =
      run_child(
        ctx.writer_state,
        ["request", binary_uri, "--input", "-" | relay_args],
        [{"ARC_TEST_STDIN", input}]
      )

    assert stdin.status == 0, stdin.output
    assert stdin.output == bytes

    occupied_output = Path.join(ctx.root, "occupied-binary-output")
    File.write!(occupied_output, "do not replace")

    occupied =
      run_child(ctx.writer_state, [
        "request",
        binary_uri,
        "--input",
        input,
        "--output",
        occupied_output
        | relay_args
      ])

    assert occupied.status != 0
    assert File.read!(occupied_output) == "do not replace"
  end

  test "--local reaches a same-host SQLite provider and a requested relay never falls back",
       ctx do
    provider_state = Path.join(ctx.root, "local-provider")
    citizen_state = Path.join(ctx.root, "local-citizen")
    provider_identity = create_identity(provider_state)
    citizen = create_identity(citizen_state)
    database = Path.join(ctx.root, "local.sqlite3")
    config = Path.join(ctx.root, "local-config.json")
    local_control = Path.join(ctx.root, "local-control")
    local_mailbox = Path.join(ctx.root, "local-mailbox")
    local_env = [{"ARC_TEST_CONTROL", local_control}, {"ARC_TEST_MAILBOX", local_mailbox}]

    File.write!(
      config,
      :json.encode(%{
        "databases" => %{
          "main" => %{"path" => database, "grants" => %{citizen.public_key => "write"}}
        }
      })
    )

    provider = start_local_provider(provider_state, config, local_env)
    on_exit(fn -> stop_and_assert_child(provider) end)
    assert_ready(provider.port, "Serving SQLite over ARC")

    # `--local` is an explicit delivery choice. A corrupt joined-relay file
    # must not affect this same-host request or cause it to consult a relay.
    File.write!(Path.join(citizen_state, "relays.json"), "not json")
    File.chmod!(Path.join(citizen_state, "relays.json"), 0o600)

    uri = "sqlite+arc://#{provider_identity.public_key}/main"

    local =
      run_child(
        citizen_state,
        [
          "request",
          uri,
          "--body",
          ~s|{"sql":"SELECT 42 AS answer"}|,
          "--local"
        ],
        local_env
      )

    assert local.status == 0, local.output
    assert %{"results" => [%{"rows" => [[42]]}]} = :json.decode(local.output)

    unavailable =
      run_child(
        citizen_state,
        [
          "request",
          uri,
          "--body",
          ~s|{"sql":"CREATE TABLE must_not_exist (id INTEGER)"}|,
          "--timeout",
          "100",
          "--relay",
          "127.0.0.1:#{ctx.absent_relay_port}",
          "--relay-pubkey",
          String.duplicate("0", 64)
        ],
        local_env
      )

    assert unavailable.status != 0
    assert File.exists?(database)
    refute File.read!(database) |> :binary.match("must_not_exist") != :nomatch
  end

  test "a direct policy cannot be combined with local delivery", ctx do
    uri = "sqlite+arc://#{String.duplicate("0", 64)}/main"
    policy = Path.join(ctx.root, "direct-policy.json")
    File.write!(policy, "not json")

    result =
      run_child(ctx.writer_state, [
        "request",
        uri,
        "--body",
        ~s|{"sql":"SELECT 1"}|,
        "--local",
        "--direct-policy",
        policy
      ])

    assert result.status != 0
    assert result.output =~ "--direct-policy requires a pinned relay"
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
    child = open_child(state_root, if(opts[:transit?], do: args ++ ["--transit"], else: args))
    {:os_pid, os_pid} = Port.info(child, :os_pid)
    %{port: child, os_pid: os_pid}
  end

  defp start_provider(state_root, config, relay_port, relay_public_key) do
    child =
      open_child(
        state_root,
        [
          "serve",
          @provider_bundle,
          "--relay",
          "127.0.0.1:#{relay_port}",
          "--relay-pubkey",
          relay_public_key,
          "--federate-network"
        ],
        [{"SQLITE_CONFIG", config}]
      )

    {:os_pid, os_pid} = Port.info(child, :os_pid)
    %{port: child, os_pid: os_pid}
  end

  defp start_local_provider(state_root, config, local_env) do
    child =
      open_child(state_root, ["serve", @provider_bundle], [{"SQLITE_CONFIG", config} | local_env])

    {:os_pid, os_pid} = Port.info(child, :os_pid)
    %{port: child, os_pid: os_pid}
  end

  defp start_raw_provider(state_root, relay_port, relay_public_key) do
    target = "exec://#{@binary_runtime}?manifest=#{@binary_manifest}"

    child =
      open_child(state_root, [
        "serve",
        target,
        "--relay",
        "127.0.0.1:#{relay_port}",
        "--relay-pubkey",
        relay_public_key,
        "--federate-network"
      ])

    {:os_pid, os_pid} = Port.info(child, :os_pid)
    %{port: child, os_pid: os_pid}
  end

  defp run_child(state_root, argv, extra \\ []) do
    state_root
    |> open_child(argv, extra)
    |> await_child(System.monotonic_time(:millisecond) + @timeout, "")
  end

  defp open_child(state_root, argv, extra \\ []) do
    mix = System.find_executable("mix") || raise "mix executable is unavailable"
    args = ["run", "--no-compile", "--no-start", @child_script, "--", state_root | argv]

    {executable, spawn_args} =
      case Enum.find(extra, fn {key, _value} -> key == "ARC_TEST_STDIN" end) do
        {"ARC_TEST_STDIN", _path} ->
          {"/bin/sh", ["-c", "exec \"$@\" < \"$ARC_TEST_STDIN\"", "arc-test", mix | args]}

        nil ->
          {mix, args}
      end

    Port.open(
      {:spawn_executable, String.to_charlist(executable)},
      [
        :binary,
        :exit_status,
        :use_stdio,
        :stderr_to_stdout,
        args: Enum.map(spawn_args, &String.to_charlist/1),
        cd: String.to_charlist(@project_root),
        env:
          ([{"MIX_ENV", "test"}, {"ARC_TEST_STATE", state_root}] ++ extra)
          |> Enum.map(fn {key, value} -> {String.to_charlist(key), String.to_charlist(value)} end)
      ]
    )
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

  defp stop_and_assert_child(%{port: port, os_pid: os_pid}) do
    assert :ok = stop_child(port)
    assert_os_pid_stopped(os_pid)
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
      assert remaining > 0, "child process #{os_pid} still running"
      Process.sleep(min(remaining, 100))
      await_os_pid_stopped(os_pid, deadline)
    end
  end

  defp unused_tcp_port do
    {:ok, socket} = :gen_tcp.listen(0, [:binary, active: false, reuseaddr: true])
    {:ok, port} = :inet.port(socket)
    :ok = :gen_tcp.close(socket)
    port
  end
end
