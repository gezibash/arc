defmodule Arc.CLI.AgoraIntegrationTest do
  use ExUnit.Case, async: false

  alias Arc.Identity

  @project_root Path.expand("../../../..", __DIR__)
  @child_script Path.join(@project_root, "test/fixtures/relay_discovery/arc_child.exs")
  @provider_bundle Path.expand("../../../../providers/agora", __DIR__)
  @timeout 30_000

  setup do
    root = Path.join(System.tmp_dir!(), "arc-agora-#{System.unique_integer([:positive])}")
    provider_state = Path.join(root, "provider")
    alice_state = Path.join(root, "alice")
    bob_state = Path.join(root, "bob")
    storage_root = Path.join(root, "board")

    File.mkdir_p!(root)

    relay_key = Identity.generate().public_key
    {:ok, relay} = Arc.Net.Relay.start_link(0, relay_public_key: relay_key)
    relay_port = Arc.Net.Relay.get_port(relay)
    relay_pin = Base.encode16(relay_key, case: :lower)

    on_exit(fn ->
      if Process.alive?(relay), do: GenServer.stop(relay, :normal)
      File.rm_rf!(root)
    end)

    %{
      provider_state: provider_state,
      alice_state: alice_state,
      bob_state: bob_state,
      storage_root: storage_root,
      relay_port: relay_port,
      relay_pin: relay_pin
    }
  end

  test "two isolated citizens install Agora and exchange signed public posts through a relay",
       ctx do
    provider_identity = create_identity(ctx.provider_state)
    alice = create_identity(ctx.alice_state)
    bob = create_identity(ctx.bob_state)
    provider = start_provider(ctx)

    on_exit(fn -> stop_child(provider.port) end)

    assert_provider_ready(provider.port, ctx.relay_port)
    relay_args = relay_args(ctx)

    for state_root <- [ctx.alice_state, ctx.bob_state] do
      assert_eventually(fn ->
        result = run_child(state_root, ["discover", "agora" | relay_args])
        result.status == 0 and result.output =~ provider_identity.name
      end)

      install =
        run_child(state_root, [
          "install",
          provider_identity.name,
          "primary",
          "--trust" | relay_args
        ])

      assert install.status == 0, install.output
      assert install.output =~ "Installed agora"
    end

    post = run_child(ctx.alice_state, ["agora", "post", "A relay-bound public post" | relay_args])
    assert post.status == 0, post.output <> provider_output(provider.port)
    post = decode_post!(post.output)
    assert post["author"] == alice.public_key
    assert post["board"] == provider_identity.public_key
    assert post["parent"] == :null
    assert post["body"] == "A relay-bound public post"
    assert byte_size(post["signature"]) == 128

    reply =
      run_child(ctx.bob_state, ["agora", "reply", post["id"], "A public reply" | relay_args])

    assert reply.status == 0, reply.output
    reply = decode_post!(reply.output)
    assert reply["author"] == bob.public_key
    assert reply["board"] == provider_identity.public_key
    assert reply["parent"] == post["id"]
    assert reply["body"] == "A public reply"
    assert byte_size(reply["signature"]) == 128

    feed = run_child(ctx.bob_state, ["agora", "feed", "--limit", "20" | relay_args])
    assert feed.status == 0, feed.output
    assert %{"posts" => [feed_post], "next" => :null} = :json.decode(feed.output)
    assert feed_post == post

    read = run_child(ctx.bob_state, ["agora", "read", post["id"] | relay_args])
    assert read.status == 0, read.output
    assert decode_post!(read.output) == post

    thread = run_child(ctx.alice_state, ["agora", "thread", post["id"] | relay_args])
    assert thread.status == 0, thread.output

    assert %{"post" => ^post, "posts" => [thread_reply], "next" => :null} =
             :json.decode(thread.output)

    assert thread_reply == reply

    for state_root <- [ctx.alice_state, ctx.bob_state] do
      mounted =
        run_child(state_root, [
          "mount",
          "public-square",
          "add",
          provider_identity.name,
          "primary" | relay_args
        ])

      assert mounted.status == 0, mounted.output
      assert mounted.output =~ "Mounted public-square"
    end

    mounted_post =
      run_child(ctx.alice_state, [
        "mount",
        "public-square",
        "call",
        provider_identity.name,
        "primary",
        "post",
        "A mounted signed post" | relay_args
      ])

    assert mounted_post.status == 0, mounted_post.output
    mounted_post = decode_post!(mounted_post.output)
    assert mounted_post["author"] == alice.public_key
    assert mounted_post["board"] == provider_identity.public_key
    assert mounted_post["parent"] == :null

    mounted_read =
      run_child(ctx.bob_state, [
        "mount",
        "public-square",
        "call",
        provider_identity.name,
        "primary",
        "read",
        mounted_post["id"] | relay_args
      ])

    assert mounted_read.status == 0, mounted_read.output
    assert decode_post!(mounted_read.output) == mounted_post

    mounted_reply =
      run_child(ctx.bob_state, [
        "mount",
        "public-square",
        "call",
        provider_identity.name,
        "primary",
        "reply",
        mounted_post["id"],
        "A mounted signed reply" | relay_args
      ])

    assert mounted_reply.status == 0, mounted_reply.output
    mounted_reply = decode_post!(mounted_reply.output)
    assert mounted_reply["author"] == bob.public_key
    assert mounted_reply["board"] == provider_identity.public_key
    assert mounted_reply["parent"] == mounted_post["id"]

    # Restarting the external runtime must retain the accepted post bytes and
    # their author signatures. The citizens still have no local control-plane
    # record for the provider, so this read also remains relay-only.
    assert :ok = stop_child(provider.port)
    assert_os_pid_stopped(provider.os_pid)
    restarted_provider = start_provider(ctx)
    on_exit(fn -> stop_child(restarted_provider.port) end)
    assert_provider_ready(restarted_provider.port, ctx.relay_port)

    read_after_restart =
      run_child(ctx.alice_state, ["agora", "read", post["id"], "--raw" | relay_args])

    assert read_after_restart.status == 0, read_after_restart.output
    assert decode_post!(read_after_restart.output) == post
  end

  defp decode_post!(output) do
    assert %{"post" => post} = :json.decode(output)
    post
  end

  defp create_identity(state_root) do
    result = run_child(state_root, ["keys", "gen"])
    assert result.status == 0, result.output

    [_, name] = Regex.run(~r/^\s*name:\s+(\S+)$/m, result.output)
    [_, public_key] = Regex.run(~r/^\s*public_key:\s+([a-f0-9]{64})$/m, result.output)
    %{name: name, public_key: public_key}
  end

  defp start_provider(ctx) do
    args = [
      "serve",
      @provider_bundle,
      "--relay",
      "127.0.0.1:#{ctx.relay_port}",
      "--relay-pubkey",
      ctx.relay_pin
    ]

    port = open_child(ctx.provider_state, args, [{"AGORA_ROOT", ctx.storage_root}])
    {:os_pid, os_pid} = Port.info(port, :os_pid)
    %{port: port, os_pid: os_pid}
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

        if output =~ "Serving Agora" and output =~ "Relay: 127.0.0.1:#{relay_port}" do
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

  defp assert_eventually(fun, timeout \\ @timeout) do
    deadline = System.monotonic_time(:millisecond) + timeout
    await_eventually(fun, deadline, nil)
  end

  defp await_eventually(fun, deadline, last_result) do
    case fun.() do
      true ->
        :ok

      false ->
        remaining = deadline - System.monotonic_time(:millisecond)
        assert remaining > 0, "condition did not become true: #{inspect(last_result)}"

        receive do
        after
          min(remaining, 250) -> await_eventually(fun, deadline, false)
        end
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

  defp provider_output(port, output \\ "") do
    receive do
      {^port, {:data, chunk}} -> provider_output(port, output <> chunk)
      {^port, {:exit_status, status}} -> output <> "\nprovider exited: #{status}\n"
    after
      25 -> output
    end
  end

  defp child_env(state_root, extra) do
    [
      {"MIX_ENV", "test"},
      {"ARC_RELAY_MAX_FRAME_BYTES", "6291456"},
      {"ARC_TEST_STATE", state_root}
      | Enum.to_list(extra)
    ]
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
