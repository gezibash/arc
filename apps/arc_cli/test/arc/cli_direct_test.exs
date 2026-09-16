defmodule Arc.CLI.DirectIntegrationTest do
  use ExUnit.Case, async: false

  alias Arc.Identity

  @project_root Path.expand("../../../..", __DIR__)
  @child_script Path.join(@project_root, "test/fixtures/protocol/arc_child.exs")
  @direct_runner Path.join(@project_root, "test/fixtures/protocol/direct_runner.exs")
  @provider_bundle Path.expand("../../../../providers/sqlite", __DIR__)
  @timeout 30_000

  setup do
    root = Path.join(System.tmp_dir!(), "arc-direct-#{System.unique_integer([:positive])}")
    File.mkdir_p!(root)
    on_exit(fn -> File.rm_rf!(root) end)

    %{
      root: root,
      relay_state: Path.join(root, "relay"),
      provider_state: Path.join(root, "provider"),
      writer_state: Path.join(root, "writer"),
      reader_state: Path.join(root, "reader"),
      relay_port: unused_tcp_port()
    }
  end

  test "a reverse-dialed SQLite route spans isolated BEAM processes and retains database grants",
       ctx do
    relay_identity = create_identity(ctx.relay_state)
    provider_identity = create_identity(ctx.provider_state)
    writer_identity = create_identity(ctx.writer_state)
    reader_identity = create_identity(ctx.reader_state)

    relay = start_relay(ctx.relay_state, relay_identity, ctx.relay_port)
    on_exit(fn -> stop_and_assert_child(relay) end)
    assert_ready(relay.port, "arc relay listening on port #{ctx.relay_port}")

    provider_policy = Path.join(ctx.root, "provider-direct-policy.json")
    writer_policy = Path.join(ctx.root, "writer-direct-policy.json")
    database = Path.join(ctx.root, "main.sqlite3")
    config = Path.join(ctx.root, "sqlite-config.json")

    # The provider can only dial approved citizens. Each citizen is the sole
    # listener, so promotion proves the provider-to-citizen reverse path across BEAM VMs.
    write_policy(provider_policy, [
      rule(writer_identity.public_key, false),
      rule(reader_identity.public_key, false)
    ])

    write_policy(writer_policy, [rule(provider_identity.public_key, true)])
    reader_policy = Path.join(ctx.root, "reader-direct-policy.json")
    write_policy(reader_policy, [rule(provider_identity.public_key, true)])

    File.write!(
      config,
      :json.encode(%{
        "databases" => %{
          "main" => %{
            "path" => database,
            "grants" => %{
              writer_identity.public_key => "write",
              reader_identity.public_key => "read"
            }
          }
        }
      })
    )

    provider =
      start_provider(
        ctx.provider_state,
        ctx.relay_port,
        relay_identity.public_key,
        provider_policy,
        config
      )

    on_exit(fn -> stop_and_assert_child(provider) end)
    assert_ready(provider.port, "Serving SQLite over ARC")

    uri = "sqlite+arc://#{provider_identity.public_key}/main"

    assert %{"results" => [%{"changes" => 0}]} =
             assert_direct_sql(
               ctx.writer_state,
               writer_policy,
               uri,
               ctx.relay_port,
               relay_identity.public_key,
               ~s|{"sql":"CREATE TABLE notes (body TEXT)"}|
             )

    assert %{"results" => [%{"changes" => 1}]} =
             assert_direct_sql(
               ctx.writer_state,
               writer_policy,
               uri,
               ctx.relay_port,
               relay_identity.public_key,
               ~s|{"sql":"INSERT INTO notes (body) VALUES (?)","params":["direct"]}|
             )

    assert %{"results" => [%{"columns" => ["body"], "rows" => [["direct"]]}]} =
             assert_direct_sql(
               ctx.writer_state,
               writer_policy,
               uri,
               ctx.relay_port,
               relay_identity.public_key,
               ~s|{"sql":"SELECT body FROM notes"}|
             )

    reader_write =
      assert_direct_error(
        ctx.reader_state,
        reader_policy,
        uri,
        ctx.relay_port,
        relay_identity.public_key,
        ~s|{"sql":"INSERT INTO notes (body) VALUES ('denied')"}|
      )

    assert reader_write["error"] =~ "write_denied"

    assert %{"results" => [%{"rows" => [["direct"]]}]} =
             assert_direct_sql(
               ctx.reader_state,
               reader_policy,
               uri,
               ctx.relay_port,
               relay_identity.public_key,
               ~s|{"sql":"SELECT body FROM notes"}|
             )
  end

  defp assert_direct_sql(state, policy, uri, relay_port, relay_key, body) do
    child =
      open_direct_runner(state, [
        policy,
        uri,
        "127.0.0.1",
        Integer.to_string(relay_port),
        relay_key,
        body
      ])

    {report, output} =
      await_direct_result(child.port, System.monotonic_time(:millisecond) + @timeout, "")

    try do
      assert report["direct"]["active"], output
      assert report["direct"]["role"] == "caller", output
      assert report["direct"]["selected"] == "caller", output
      assert report["direct"]["remaining_ms"] > 0, output
      assert report["result"]["ok"], output
      report["result"]["body"]
    after
      stop_and_assert_child(child)
    end
  end

  defp assert_direct_error(state, policy, uri, relay_port, relay_key, body) do
    child =
      open_direct_runner(state, [
        policy,
        uri,
        "127.0.0.1",
        Integer.to_string(relay_port),
        relay_key,
        body
      ])

    {report, output} =
      await_direct_result(child.port, System.monotonic_time(:millisecond) + @timeout, "")

    try do
      assert report["direct"]["active"], output
      assert report["direct"]["role"] == "caller", output
      assert report["direct"]["selected"] == "caller", output
      refute report["result"]["ok"], output
      report["result"]
    after
      stop_and_assert_child(child)
    end
  end

  defp rule(peer, listen?) do
    rule = %{
      "peer" => peer,
      "capability" => "primary",
      "scheme" => "sqlite",
      "path" => "/main",
      "lease_ms" => 10_000,
      "dial" => ["127.0.0.1"]
    }

    if listen? do
      Map.put(rule, "listen", %{"bind" => "127.0.0.1", "address" => "127.0.0.1", "port" => 0})
    else
      rule
    end
  end

  defp write_policy(path, rules),
    do: File.write!(path, :json.encode(%{"version" => 1, "rules" => rules}))

  defp create_identity(state_root) do
    identity = Identity.generate()
    name = Identity.name(identity)
    keys_dir = Path.join(state_root, "keys")
    File.mkdir_p!(keys_dir)

    File.write!(
      Path.join(keys_dir, "#{name}.toml"),
      "[identity]\nseed = \"#{Base.encode16(identity.seed, case: :lower)}\"\n"
    )

    File.write!(Path.join(state_root, "default_key"), name)
    %{name: name, public_key: Identity.encode_public_key(identity)}
  end

  defp start_relay(state_root, identity, port) do
    child =
      open_child(state_root, ["relay", "--port", Integer.to_string(port), "--key", identity.name])

    {:os_pid, os_pid} = Port.info(child, :os_pid)
    %{port: child, os_pid: os_pid}
  end

  defp start_provider(state_root, relay_port, relay_key, policy, config) do
    child =
      open_child(
        state_root,
        [
          "serve",
          @provider_bundle,
          "--relay",
          "127.0.0.1:#{relay_port}",
          "--relay-pubkey",
          relay_key,
          "--direct-policy",
          policy
        ],
        [{"SQLITE_CONFIG", config}]
      )

    {:os_pid, os_pid} = Port.info(child, :os_pid)
    %{port: child, os_pid: os_pid}
  end

  defp open_child(state_root, argv, extra \\ []) do
    mix = System.find_executable("mix") || raise "mix executable is unavailable"
    args = ["run", "--no-compile", "--no-start", @child_script, "--", state_root | argv]
    open_process(mix, args, state_root, extra)
  end

  defp open_direct_runner(state_root, args) do
    mix = System.find_executable("mix") || raise "mix executable is unavailable"
    argv = ["run", "--no-compile", "--no-start", @direct_runner, "--", state_root | args]
    port = open_process(mix, argv, state_root, [])
    {:os_pid, os_pid} = Port.info(port, :os_pid)
    %{port: port, os_pid: os_pid}
  end

  defp open_process(executable, args, state_root, extra) do
    Port.open(
      {:spawn_executable, String.to_charlist(executable)},
      [
        :binary,
        :exit_status,
        :use_stdio,
        :stderr_to_stdout,
        args: Enum.map(args, &String.to_charlist/1),
        cd: String.to_charlist(@project_root),
        env:
          ([{"MIX_ENV", "test"}, {"ARC_TEST_STATE", state_root}] ++ extra)
          |> Enum.map(fn {key, value} -> {String.to_charlist(key), String.to_charlist(value)} end)
      ]
    )
  end

  defp assert_ready(port, expected),
    do: await_ready(port, expected, System.monotonic_time(:millisecond) + @timeout, "")

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

  defp await_direct_result(port, deadline, output) do
    remaining = deadline - System.monotonic_time(:millisecond)
    assert remaining > 0, "direct runner did not report:\n#{output}"

    receive do
      {^port, {:data, chunk}} ->
        output = output <> chunk

        case Regex.run(~r/ARC_DIRECT_RESULT (\{[^\n]+\})/, output) do
          [_, json] -> {:json.decode(json), output}
          _ -> await_direct_result(port, deadline, output)
        end

      {^port, {:exit_status, status}} ->
        flunk("direct runner exited (#{status}):\n#{output}")
    after
      min(remaining, 250) -> await_direct_result(port, deadline, output)
    end
  end

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
