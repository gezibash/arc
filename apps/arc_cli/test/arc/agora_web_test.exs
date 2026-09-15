defmodule Arc.CLI.AgoraWebTest do
  use ExUnit.Case, async: false

  alias Arc.CLI.{AgoraWeb, ToolRegistry}
  alias Arc.Data.CapabilityPackage
  alias Arc.Identity

  @manifest Path.expand("../../../../providers/agora/manifest.json", __DIR__)

  setup do
    root = Path.join(System.tmp_dir!(), "agora-web-#{System.unique_integer([:positive])}")
    previous = Application.fetch_env(:arc_cli, :tool_registry_dir)
    Application.put_env(:arc_cli, :tool_registry_dir, Path.join(root, "tools"))

    on_exit(fn ->
      case previous do
        {:ok, value} -> Application.put_env(:arc_cli, :tool_registry_dir, value)
        :error -> Application.delete_env(:arc_cli, :tool_registry_dir)
      end

      File.rm_rf!(root)
    end)

    identity = Identity.generate()
    {:ok, package} = CapabilityPackage.load_file(@manifest)
    signed = CapabilityPackage.sign(Identity.generate(), package)
    {:ok, tool} = ToolRegistry.install(identity, signed)
    {:ok, server} = AgoraWeb.start_link(identity: identity, tool: tool, port: 0)
    Process.unlink(server)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
    end)

    %{server: server, owner_public_key: identity.public_key, tool: tool}
  end

  test "stopping the browser releases its listener and citizen agent", ctx do
    port = AgoraWeb.port(ctx.server)
    [{agent, _}] = Registry.lookup(Arc.Data.AgentRegistry, ctx.owner_public_key)
    assert :sys.get_state(ctx.server).agent == agent
    monitor = Process.monitor(agent)
    assert :ok = GenServer.stop(ctx.server, :normal)
    refute Process.alive?(agent), "citizen agent survived local server shutdown"
    assert_receive {:DOWN, ^monitor, :process, ^agent, :normal}, 1_000
    await_unregistered(ctx.owner_public_key)
    assert {:error, :econnrefused} = :gen_tcp.connect({127, 0, 0, 1}, port, [], 1_000)
  end

  test "a busy browser port does not leave a citizen agent running", ctx do
    Process.flag(:trap_exit, true)
    citizen = Identity.generate()

    result =
      AgoraWeb.start_link(identity: citizen, tool: ctx.tool, port: AgoraWeb.port(ctx.server))

    assert result == {:error, :eaddrinuse}
    await_unregistered(citizen.public_key)
  end

  test "relay startup failure cleans up instead of switching to local mode", ctx do
    Process.flag(:trap_exit, true)
    citizen = Identity.generate()
    {:ok, listener} = :gen_tcp.listen(0, ip: {127, 0, 0, 1})
    {:ok, relay_port} = :inet.port(listener)
    :gen_tcp.close(listener)

    result =
      AgoraWeb.start_link(
        identity: citizen,
        tool: ctx.tool,
        port: 0,
        relay: {~c"127.0.0.1", relay_port}
      )

    assert match?({:error, _}, result)
    await_unregistered(citizen.public_key)
  end

  test "requires a one-use local launch token before serving session data", %{server: server} do
    port = AgoraWeb.port(server)
    assert {401, _headers, _body} = request(port, "GET", "/api/session")

    token =
      AgoraWeb.launch_url(server)
      |> URI.parse()
      |> Map.fetch!(:fragment)
      |> String.replace_prefix("token=", "")

    body = :json.encode(%{"token" => token}) |> IO.iodata_to_binary()

    assert {200, headers, body} =
             request(port, "POST", "/api/session", body, [{"content-type", "application/json"}])

    assert body =~ "citizen"
    cookie = headers["set-cookie"]
    assert is_binary(cookie)
    assert {200, _headers, body} = request(port, "GET", "/api/session", "", [{"cookie", cookie}])
    assert body =~ "body_bytes"

    assert {401, _headers, _body} =
             request(
               port,
               "POST",
               "/api/session",
               :json.encode(%{"token" => token}) |> IO.iodata_to_binary(),
               [{"content-type", "application/json"}]
             )
  end

  test "rejects cross-origin, malformed, and non-JSON browser requests", %{server: server} do
    port = AgoraWeb.port(server)
    cookie = bootstrap_cookie(server, port)

    assert {403, _headers, _body} =
             request(port, "GET", "/api/feed", "", [
               {"cookie", cookie},
               {"origin", "https://elsewhere.example"}
             ])

    assert {400, _headers, _body} =
             request(port, "POST", "/api/posts", "{}", [{"cookie", cookie}])

    assert {400, _headers, _body} =
             request(port, "POST", "/api/posts", "{}", [
               {"cookie", cookie},
               {"content-type", "application/json"},
               {"transfer-encoding", "chunked"}
             ])

    assert {400, _headers, _body} =
             request(port, "GET", "/api/session", "", [{"host", "127.0.0.1:1"}])

    assert {413, _headers, _body} =
             request(port, "GET", "/api/session", "", [{"x-large", String.duplicate("a", 17_000)}])
  end

  test "a slow provider does not kill the browser listener" do
    %{server: server, provider: provider, log: log, root: root} =
      start_browser_with_provider("slow")

    try do
      port = AgoraWeb.port(server)
      cookie = bootstrap_cookie(server, port)
      pending = Task.async(fn -> post(port, cookie, "slow-request-0001", "A slow post") end)
      await_file(log, 200)

      # This arrives while the post occupies the server for longer than the
      # old accept timeout. The current request and later requests must survive.
      assert {200, _, _} = request(port, "GET", "/api/session", "", [{"cookie", cookie}])
      assert {200, _, _} = Task.await(pending, 5_000)
      assert {200, _, _} = request(port, "GET", "/api/session", "", [{"cookie", cookie}])
    after
      stop_server_and_provider(server, provider, root)
    end
  end

  test "retries the same signed envelope and caches its verified receipt" do
    %{server: server, provider: provider, log: log, root: root} =
      start_browser_with_provider("flaky")

    try do
      port = AgoraWeb.port(server)
      cookie = bootstrap_cookie(server, port)
      request_id = "retry-request-0001"
      post_body = "--hello\nworld"

      assert {400, _headers, first_body} = post(port, cookie, request_id, post_body)
      assert first_body =~ "temporary provider error"
      assert {200, _headers, body} = post(port, cookie, request_id, post_body)
      assert %{"post" => _post} = :json.decode(body)

      [first, second] = File.read!(log) |> String.split("\n", trim: true)
      assert first == second

      assert %{"post" => %{"body" => ^post_body, "id" => id, "signature" => signature}} =
               :json.decode(first)

      assert is_binary(id) and is_binary(signature)

      assert {200, _headers, ^body} = post(port, cookie, request_id, post_body)
      assert length(File.read!(log) |> String.split("\n", trim: true)) == 2

      assert {400, _headers, _body} = post(port, cookie, request_id, "Changed body")

      assert {400, _headers, _body} =
               post(port, cookie, request_id, post_body, String.duplicate("a", 64))
    after
      stop_server_and_provider(server, provider, root)
    end
  end

  test "does not expose a malformed provider receipt" do
    %{server: server, provider: provider, root: root} = start_browser_with_provider("forged")

    try do
      port = AgoraWeb.port(server)
      cookie = bootstrap_cookie(server, port)

      assert {400, _headers, _body} =
               post(port, cookie, "forged-receipt-01", "Reject forged replies")
    after
      stop_server_and_provider(server, provider, root)
    end
  end

  defp bootstrap_cookie(server, port) do
    token =
      AgoraWeb.launch_url(server)
      |> URI.parse()
      |> Map.fetch!(:fragment)
      |> String.replace_prefix("token=", "")

    body = :json.encode(%{"token" => token}) |> IO.iodata_to_binary()

    assert {200, headers, _body} =
             request(port, "POST", "/api/session", body, [{"content-type", "application/json"}])

    headers["set-cookie"]
  end

  defp post(port, cookie, request_id, body, parent \\ nil) do
    parent = if(parent == nil, do: :null, else: parent)

    body =
      :json.encode(%{"body" => body, "parent" => parent, "request_id" => request_id})
      |> IO.iodata_to_binary()

    request(port, "POST", "/api/posts", body, [
      {"cookie", cookie},
      {"content-type", "application/json"}
    ])
  end

  defp start_browser_with_provider(mode) do
    root =
      Path.join(System.tmp_dir!(), "agora-web-provider-#{System.unique_integer([:positive])}")

    File.mkdir_p!(root)
    runtime = Path.join(root, "provider.exs")
    log = Path.join(root, "requests.jsonl")
    count = Path.join(root, "count")
    File.write!(runtime, provider_runtime())
    File.chmod!(runtime, 0o755)

    board = Identity.generate()
    citizen = Identity.generate()
    {:ok, package} = CapabilityPackage.load_file(@manifest)
    signed = CapabilityPackage.sign(board, package)

    {:ok, tool} =
      ToolRegistry.install(citizen, signed,
        command: "agora-#{System.unique_integer([:positive])}"
      )

    query =
      URI.encode_query(%{
        "manifest" => @manifest,
        "args" => :json.encode([mode, count, log]) |> IO.iodata_to_binary()
      })

    {:ok, provider} = Arc.Data.Agent.start_link(board, serve: "exec://#{runtime}?#{query}")
    :ok = Arc.Data.Agent.publish(provider)
    {:ok, server} = AgoraWeb.start_link(identity: citizen, tool: tool, port: 0)

    %{server: server, provider: provider, log: log, root: root}
  end

  defp stop_server_and_provider(server, provider, root) do
    if Process.alive?(server), do: GenServer.stop(server, :normal)
    if Process.alive?(provider), do: GenServer.stop(provider, :normal)
    File.rm_rf!(root)
  end

  defp provider_runtime do
    """
    #!/usr/bin/env elixir
    [mode, count_path, log_path] = System.argv()

    for line <- IO.stream(:stdio, :line) do
      event = line |> String.trim() |> :json.decode()
      message = event["message"]
      File.write!(log_path, message <> "\\n", [:append])
      count = if File.exists?(count_path), do: String.to_integer(File.read!(count_path)), else: 0
      File.write!(count_path, Integer.to_string(count + 1))
      if mode == "slow", do: Process.sleep(1_500)

      response =
        case {mode, count} do
          {"flaky", 0} -> %{"error" => "temporary provider error"}
          {"forged", _} ->
            post = message |> :json.decode() |> Map.fetch!("post") |> Map.put("board", String.duplicate("0", 64))
            %{"reply" => :json.encode(%{"post" => post}) |> IO.iodata_to_binary()}
          _ ->
            post = message |> :json.decode() |> Map.fetch!("post")
            %{"reply" => :json.encode(%{"post" => post}) |> IO.iodata_to_binary()}
        end

      IO.binwrite((:json.encode(response) |> IO.iodata_to_binary()) <> "\\n")
    end
    """
  end

  defp request(port, method, path, body \\ "", headers \\ []) do
    {:ok, socket} = :gen_tcp.connect({127, 0, 0, 1}, port, [:binary, active: false], 2_000)

    request = [
      method,
      " ",
      path,
      " HTTP/1.1\r\nHost: 127.0.0.1:",
      Integer.to_string(port),
      "\r\nContent-Length: ",
      Integer.to_string(byte_size(body)),
      "\r\n",
      Enum.map(headers, fn {key, value} -> [key, ": ", value, "\r\n"] end),
      "\r\n",
      body
    ]

    :ok = :gen_tcp.send(socket, request)
    response = receive_all(socket, "")
    :ok = :gen_tcp.close(socket)
    [head, response_body] = String.split(response, "\r\n\r\n", parts: 2)
    ["HTTP/1.1 " <> status_line | lines] = String.split(head, "\r\n")
    {status, ""} = Integer.parse(String.slice(status_line, 0, 3))

    parsed_headers =
      Map.new(lines, fn line ->
        [key, value] = String.split(line, ":", parts: 2)
        {String.downcase(key), String.trim(value)}
      end)

    {status, parsed_headers, response_body}
  end

  defp await_file(path, attempts) when attempts > 0 do
    if File.exists?(path) do
      :ok
    else
      Process.sleep(10)
      await_file(path, attempts - 1)
    end
  end

  defp await_file(_path, 0), do: flunk("provider did not receive the request")

  # Registry removes entries when its own monitor receives DOWN, independently
  # of this test's monitor. The agent must already be dead before waiting here.
  defp await_unregistered(public_key, remaining \\ 100)

  defp await_unregistered(public_key, remaining) when remaining > 0 do
    case Registry.lookup(Arc.Data.AgentRegistry, public_key) do
      [] ->
        :ok

      [{agent, _}] ->
        refute Process.alive?(agent), "citizen agent remained alive after cleanup"
        Process.sleep(10)
        await_unregistered(public_key, remaining - 1)
    end
  end

  defp await_unregistered(_public_key, 0), do: flunk("citizen remained in the local registry")

  defp receive_all(socket, acc) do
    case :gen_tcp.recv(socket, 0, 5_000) do
      {:ok, chunk} -> receive_all(socket, acc <> chunk)
      {:error, :closed} -> acc
      {:error, reason} -> flunk("browser response failed: #{inspect(reason)}")
    end
  end
end
