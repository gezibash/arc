defmodule Arc.Data.Handler.ExecTest do
  use ExUnit.Case, async: false

  alias Arc.Data.Handler
  alias Arc.Data.Handler.Exec

  @from_pk <<1::256>>

  test "spawns subprocess and gets reply" do
    {path, manifest_path} = hello_provider_paths()
    File.chmod!(path, 0o755)
    {:ok, state} = Exec.init("exec://#{path}?manifest=#{URI.encode_www_form(manifest_path)}")

    {:noreply, state} =
      Exec.handle_message("hello", @from_pk, %{request_id: <<1::128>>, framed?: true}, state)

    line = ~s({"reply":"provider hello from #{Base.encode16(@from_pk, case: :lower)}: hello"})
    assert {:emit, [event], _state} = Exec.handle_info({state.port, {:data, {:eol, line}}}, state)
    assert event.frame_type == :response
    assert event.body =~ "provider hello"
  end

  test "reply lines larger than the read chunk are reassembled" do
    {path, manifest_path} = hello_provider_paths()
    File.chmod!(path, 0o755)
    {:ok, state} = Exec.init("exec://#{path}?manifest=#{URI.encode_www_form(manifest_path)}")

    body = String.duplicate("a", 3_000_000)

    {:noreply, state} =
      Exec.handle_message(
        "POST /echo " <> body,
        @from_pk,
        %{request_id: <<7::128>>, framed?: true},
        state
      )

    assert {:emit, [event], state} = drain_until_event(state, 30_000)
    assert event.frame_type == :response
    assert event.body == body
    assert state.line_buffer == []
  end

  test "a line over the cap fails the oldest pending request and is dropped" do
    {path, manifest_path} = hello_provider_paths()
    File.chmod!(path, 0o755)
    {:ok, state} = Exec.init("exec://#{path}?manifest=#{URI.encode_www_form(manifest_path)}")

    {:noreply, state} =
      Exec.handle_message("big", @from_pk, %{request_id: <<8::128>>, framed?: true}, state)

    chunk = String.duplicate("x", 1024 * 1024)

    state =
      Enum.reduce(1..64, state, fn _, state ->
        {:noreply, state} = Exec.handle_info({state.port, {:data, {:noeol, chunk}}}, state)
        state
      end)

    assert {:emit, [event], state} =
             Exec.handle_info({state.port, {:data, {:noeol, "x"}}}, state)

    assert event.frame_type == :error
    assert event.meta["message"] =~ "exceeds"
    assert state.line_buffer == :discard

    {:noreply, state} = Exec.handle_info({state.port, {:data, {:noeol, "more"}}}, state)
    {:noreply, state} = Exec.handle_info({state.port, {:data, {:eol, "tail"}}}, state)
    assert state.line_buffer == []
    assert state.line_buffer_bytes == 0
    assert state.pending_requests == %{}

    # The next line decodes cleanly.
    {:noreply, state} =
      Exec.handle_message("after", @from_pk, %{request_id: <<9::128>>, framed?: true}, state)

    line =
      ~s({"op":"reply","request_id":"#{Base.encode16(<<9::128>>, case: :lower)}","reply":"ok"})

    assert {:emit, [event], _} = Exec.handle_info({state.port, {:data, {:eol, line}}}, state)
    assert event.body == "ok"
  end

  defp drain_until_event(state, timeout) do
    deadline = System.monotonic_time(:millisecond) + timeout

    receive do
      {port, {:data, _}} = msg when port == state.port ->
        case Exec.handle_info(msg, state) do
          {:noreply, state} ->
            drain_until_event(state, deadline - System.monotonic_time(:millisecond))

          result ->
            result
        end
    after
      max(timeout, 0) -> flunk("expected provider reply before timeout")
    end
  end

  test "from_pk is passed as hex" do
    {path, manifest_path} = hello_provider_paths()
    File.chmod!(path, 0o755)
    {:ok, state} = Exec.init("exec://#{path}?manifest=#{URI.encode_www_form(manifest_path)}")
    from_hex = Base.encode16(@from_pk, case: :lower)

    {:noreply, state} =
      Exec.handle_message("GET /from", @from_pk, %{request_id: <<2::128>>, framed?: true}, state)

    line = ~s({"reply":"provider hello from #{from_hex}: GET /from"})
    assert {:emit, [event], _state} = Exec.handle_info({state.port, {:data, {:eol, line}}}, state)
    assert event.body =~ from_hex
  end

  test "subprocess stays alive across multiple messages" do
    {path, manifest_path} = hello_provider_paths()
    File.chmod!(path, 0o755)
    {:ok, state} = Exec.init("exec://#{path}?manifest=#{URI.encode_www_form(manifest_path)}")

    {:noreply, state} =
      Exec.handle_message("first", @from_pk, %{request_id: <<3::128>>, framed?: true}, state)

    line1 =
      ~s({"op":"reply","request_id":"#{Base.encode16(<<3::128>>, case: :lower)}","reply":"first"})

    assert {:emit, [event1], state} =
             Exec.handle_info({state.port, {:data, {:eol, line1}}}, state)

    {:noreply, state} =
      Exec.handle_message("second", @from_pk, %{request_id: <<4::128>>, framed?: true}, state)

    line2 =
      ~s({"op":"reply","request_id":"#{Base.encode16(<<4::128>>, case: :lower)}","reply":"second"})

    assert {:emit, [event2], _state} =
             Exec.handle_info({state.port, {:data, {:eol, line2}}}, state)

    assert event1.body =~ "first"
    assert event2.body =~ "second"
  end

  test "exec can load a provider-authored manifest and forward full request context" do
    {runtime_path, manifest_path} = context_provider_paths()
    File.chmod!(runtime_path, 0o755)

    {:ok, state} =
      Exec.init("exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}")

    assert {:ok, package} = Handler.validate_served_capability(Exec, state)
    assert package["capability"]["scheme"] == "ctx"

    request_id = <<2::128>>

    {:noreply, state} =
      Exec.handle_message(
        "GET /",
        @from_pk,
        %{
          meta: %{"method" => "GET"},
          request_id: request_id,
          app_session_id: "demo-session",
          framed?: true
        },
        state
      )

    line =
      ~s({"op":"reply","request_id":"#{Base.encode16(request_id, case: :lower)}","reply":{"message":"GET /","from":"#{Base.encode16(@from_pk, case: :lower)}","method":"GET","request_id":"#{Base.encode16(request_id, case: :lower)}","app_session_id":"demo-session","framed":true}})

    assert {:emit, [event], _state} = Exec.handle_info({state.port, {:data, {:eol, line}}}, state)

    decoded = :json.decode(event.body)
    assert decoded["message"] == "GET /"
    assert decoded["from"] == Base.encode16(@from_pk, case: :lower)
    assert decoded["method"] == "GET"
    assert decoded["request_id"] == Base.encode16(request_id, case: :lower)
    assert decoded["app_session_id"] == "demo-session"
    assert decoded["framed"] == true
  end

  test "init returns error for missing binary" do
    assert {:error, {:not_found, _}} = Exec.init("exec:///nonexistent/binary")
  end

  test "init requires an explicit manifest" do
    {path, _manifest_path} = hello_provider_paths()
    assert {:error, :missing_manifest} = Exec.init("exec://#{path}")
  end

  test "exec supports stream events" do
    {runtime_path, manifest_path} = sandbox_provider_paths()
    File.chmod!(runtime_path, 0o755)

    {:ok, state} =
      Exec.init("exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}")

    {:noreply, state} =
      Exec.handle_frame(
        :stream_open,
        "SHELL sb-1",
        @from_pk,
        %{request_id: <<5::128>>, app_session_id: "app-1", framed?: true, meta: %{}},
        state
      )

    line_open = ~s({"op":"stream_data","app_session_id":"app-1","data":"opened"})

    assert {:emit, [open_event], state} =
             Exec.handle_info({state.port, {:data, {:eol, line_open}}}, state)

    assert open_event.frame_type == :stream_data
    assert open_event.meta["app_session_id"] == "app-1"

    {:noreply, state} =
      Exec.handle_frame(
        :stream_data,
        "exit\n",
        @from_pk,
        %{request_id: <<5::128>>, app_session_id: "app-1", framed?: true, meta: %{}},
        state
      )

    line_exit = ~s({"op":"stream_exit","app_session_id":"app-1","status":0})

    assert {:emit, [exit_event], _state} =
             Exec.handle_info({state.port, {:data, {:eol, line_exit}}}, state)

    assert exit_event.frame_type == :stream_exit
    assert exit_event.meta["status"] == 0
  end

  test "exec injects ARC host environment variables into provider runtimes" do
    {runtime_path, manifest_path} = env_provider_paths()
    File.chmod!(runtime_path, 0o755)

    {:ok, state} =
      Exec.init(
        "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}&arc_host_socket=#{URI.encode_www_form("/tmp/arc-host.sock")}&arc_host_token=#{URI.encode_www_form("host-token-123")}&arc_identity=agile-fresnel-dcadbb2e&arc_identity_short=agile-fresnel&arc_public_key=#{String.duplicate("ab", 32)}"
      )

    {:noreply, state} =
      Exec.handle_message("env", @from_pk, %{request_id: <<6::128>>, framed?: true}, state)

    line =
      receive do
        {port, {:data, {:eol, line}}} when port == state.port -> line
      after
        30_000 -> flunk("expected provider env reply")
      end

    assert {:emit, [event], _state} = Exec.handle_info({state.port, {:data, {:eol, line}}}, state)

    decoded = :json.decode(event.body)
    assert decoded["ARC_HOST_SOCKET"] == "/tmp/arc-host.sock"
    assert decoded["ARC_HOST_TOKEN"] == "host-token-123"
    assert decoded["ARC_IDENTITY"] == "agile-fresnel-dcadbb2e"
    assert decoded["ARC_IDENTITY_SHORT"] == "agile-fresnel"
    assert decoded["ARC_PUBLIC_KEY"] == String.duplicate("ab", 32)
  end

  defp hello_provider_paths do
    runtime = Path.expand("../../../../../../test/fixtures/providers/hello-provider.exs", __DIR__)

    manifest =
      Path.expand("../../../../../../test/fixtures/providers/hello-provider.json", __DIR__)

    {runtime, manifest}
  end

  defp context_provider_paths do
    runtime =
      Path.expand("../../../../../../test/fixtures/providers/context-provider.exs", __DIR__)

    manifest =
      Path.expand("../../../../../../test/fixtures/providers/context-provider.json", __DIR__)

    {runtime, manifest}
  end

  defp sandbox_provider_paths do
    runtime =
      Path.expand("../../../../../../test/fixtures/providers/sandbox-provider.exs", __DIR__)

    manifest =
      Path.expand("../../../../../../test/fixtures/providers/sandbox-provider.json", __DIR__)

    {runtime, manifest}
  end

  defp env_provider_paths do
    runtime = Path.expand("../../../../../../test/fixtures/providers/env-provider.exs", __DIR__)
    manifest = Path.expand("../../../../../../test/fixtures/providers/env-provider.json", __DIR__)
    {runtime, manifest}
  end
end
