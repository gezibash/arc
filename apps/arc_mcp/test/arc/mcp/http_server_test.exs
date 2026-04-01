defmodule Arc.MCP.HTTPServerTest do
  use ExUnit.Case, async: false

  alias Arc.Data.Agent
  alias Arc.Identity
  alias Arc.Identity.KeyStore
  alias Arc.MCP.DynamicToolRegistry
  alias Arc.MCP.HTTPServer

  @auth_context "ARC-MCP-AUTH-V1"

  setup do
    Application.ensure_all_started(:arc_net)
    Arc.Control.Local.reset()
    Arc.Net.TransportManager.reset()

    dir =
      Path.join(
        System.tmp_dir!(),
        "arc_http_mcp_#{System.unique_integer([:positive, :monotonic])}"
      )

    File.mkdir_p!(dir)

    on_exit(fn ->
      File.rm_rf!(dir)
      Arc.Net.TransportManager.reset()
    end)

    %{dir: dir}
  end

  test "different authenticated identities receive different task toolboxes", %{dir: dir} do
    first_owner = persist_identity()
    second_owner = persist_identity()

    assert {:ok, _} =
             DynamicToolRegistry.mount(
               first_owner,
               "demo",
               detail_doc("alpha", "primary"),
               dir: dir
             )

    assert {:ok, _} =
             DynamicToolRegistry.mount(
               second_owner,
               "demo",
               detail_doc("beta", "secondary"),
               dir: dir
             )

    {:ok, server} = HTTPServer.start_link(task: "demo", port: 0, registry_opts: [dir: dir])

    on_exit(fn ->
      stop_process(server)
      KeyStore.remove(Identity.name(first_owner))
      KeyStore.remove(Identity.name(second_owner))
    end)

    {first_session_id, protocol_version} = initialize_session(server, first_owner)
    {second_session_id, ^protocol_version} = initialize_session(server, second_owner)

    first_tools =
      server
      |> tools_list(first_session_id, protocol_version)
      |> Enum.map(& &1["name"])

    second_tools =
      server
      |> tools_list(second_session_id, protocol_version)
      |> Enum.map(& &1["name"])

    assert "send" in first_tools
    assert "send" in second_tools
    assert "arc_alpha__primary" in first_tools
    refute "arc_beta__secondary" in first_tools
    assert "arc_beta__secondary" in second_tools
    refute "arc_alpha__primary" in second_tools
  end

  test "authenticated session can call the default send tool", %{dir: dir} do
    owner = persist_identity()
    receiver = Identity.generate()

    {:ok, receiver_agent} = Agent.start_link(receiver)
    :ok = Agent.publish(receiver_agent)

    {:ok, server} = HTTPServer.start_link(task: "demo", port: 0, registry_opts: [dir: dir])

    on_exit(fn ->
      stop_process(server)
      stop_process(receiver_agent)
      KeyStore.remove(Identity.name(owner))
    end)

    {session_id, protocol_version} = initialize_session(server, owner)

    result =
      tools_call(server, session_id, protocol_version, "send", %{
        "to" => Identity.name(receiver),
        "message" => "hello from http mcp",
        "await_reply" => false
      })

    assert result["isError"] == false
    assert get_in(result, ["content", Access.at(0), "text"]) =~ "Sent to"

    Agent.poll_mailbox(receiver_agent)
    Process.sleep(25)

    assert [%{from: from, text: "hello from http mcp"}] = Agent.read_inbox(receiver_agent)
    assert from == Identity.name(owner)
  end

  test "mounted tool changes trigger tools/list_changed after initialization", %{dir: dir} do
    owner = persist_identity()

    {:ok, server} =
      HTTPServer.start_link(task: "demo", port: 0, poll_ms: 50, registry_opts: [dir: dir])

    on_exit(fn ->
      stop_process(server)
      KeyStore.remove(Identity.name(owner))
    end)

    {session_id, protocol_version} = initialize_session(server, owner)
    :ok = send_initialized(server, session_id, protocol_version)

    {:ok, socket} = open_sse(server, session_id, protocol_version)

    assert_receive {:sse_ready, ^socket}, 1_000

    assert {:ok, _} =
             DynamicToolRegistry.mount(
               owner,
               "demo",
               detail_doc("alpha", "primary"),
               dir: dir
             )

    assert {:ok, event} = recv_until(socket, "notifications/tools/list_changed", 2_000)
    assert event =~ "notifications/tools/list_changed"

    :gen_tcp.close(socket)
  end

  test "deleting sessions releases leased relay transports after idle timeout", %{dir: dir} do
    first_owner = persist_identity()
    second_owner = persist_identity()

    {:ok, relay} = Arc.Net.Relay.start_link(0)
    relay_port = Arc.Net.Relay.get_port(relay)

    {:ok, server} =
      HTTPServer.start_link(
        task: "demo",
        port: 0,
        relay: {~c"localhost", relay_port},
        relay_idle_timeout_ms: 100,
        registry_opts: [dir: dir]
      )

    on_exit(fn ->
      stop_process(server)
      stop_process(relay)
      KeyStore.remove(Identity.name(first_owner))
      KeyStore.remove(Identity.name(second_owner))
    end)

    {first_session_id, protocol_version} = initialize_session(server, first_owner)
    {second_session_id, ^protocol_version} = initialize_session(server, second_owner)

    assert wait_until(fn -> Arc.Net.TransportManager.count() == 2 end)
    assert wait_until(fn -> Arc.Net.Relay.stats(relay).conns == 2 end)

    assert delete_session(server, first_session_id, protocol_version).status == 204
    assert wait_until(fn -> Arc.Net.TransportManager.count() == 1 end)
    assert wait_until(fn -> Arc.Net.Relay.stats(relay).conns == 1 end)

    assert delete_session(server, second_session_id, protocol_version).status == 204
    assert wait_until(fn -> Arc.Net.TransportManager.count() == 0 end)
    assert wait_until(fn -> Arc.Net.Relay.stats(relay).conns == 0 end)
  end

  defp persist_identity do
    identity = Identity.generate()
    :ok = KeyStore.save(identity)
    identity
  end

  defp initialize_session(server, identity) do
    request = %{
      "jsonrpc" => "2.0",
      "id" => 1,
      "method" => "initialize",
      "params" => %{"protocolVersion" => "2025-11-25", "capabilities" => %{}}
    }

    body = encode_json(request)
    response = http_request(server, "POST", auth_headers(identity, "/mcp", body), body)

    assert response.status == 200

    {
      Map.fetch!(response.headers, "mcp-session-id"),
      Map.fetch!(response.headers, "mcp-protocol-version")
    }
  end

  defp tools_list(server, session_id, protocol_version) do
    request = %{"jsonrpc" => "2.0", "id" => 2, "method" => "tools/list", "params" => %{}}

    response =
      http_request(
        server,
        "POST",
        session_headers(session_id, protocol_version),
        encode_json(request)
      )

    assert response.status == 200
    get_in(decode_json(response.body), ["result", "tools"])
  end

  defp tools_call(server, session_id, protocol_version, tool_name, arguments) do
    request = %{
      "jsonrpc" => "2.0",
      "id" => 3,
      "method" => "tools/call",
      "params" => %{"name" => tool_name, "arguments" => arguments}
    }

    response =
      http_request(
        server,
        "POST",
        session_headers(session_id, protocol_version),
        encode_json(request)
      )

    assert response.status == 200
    get_in(decode_json(response.body), ["result"])
  end

  defp send_initialized(server, session_id, protocol_version) do
    request = %{"jsonrpc" => "2.0", "method" => "notifications/initialized", "params" => %{}}

    response =
      http_request(
        server,
        "POST",
        session_headers(session_id, protocol_version),
        encode_json(request)
      )

    assert response.status == 202
    :ok
  end

  defp delete_session(server, session_id, protocol_version) do
    http_request(server, "DELETE", session_headers(session_id, protocol_version), "")
  end

  defp open_sse(server, session_id, protocol_version) do
    {:ok, socket} =
      :gen_tcp.connect({127, 0, 0, 1}, HTTPServer.port(server), [
        :binary,
        active: false,
        packet: :raw
      ])

    request = [
      "GET /mcp HTTP/1.1\r\n",
      "Host: localhost\r\n",
      "Accept: text/event-stream\r\n",
      "Mcp-Session-Id: ",
      session_id,
      "\r\n",
      "Mcp-Protocol-Version: ",
      protocol_version,
      "\r\n",
      "\r\n"
    ]

    :ok = :gen_tcp.send(socket, request)

    parent = self()

    Task.start(fn ->
      {:ok, ready} = recv_until(socket, ": connected", 2_000)
      send(parent, {:sse_ready, socket})
      ready
    end)

    {:ok, socket}
  end

  defp auth_headers(identity, path, body) do
    timestamp = System.system_time(:millisecond)
    nonce = Base.url_encode64(:crypto.strong_rand_bytes(12), padding: false)

    message =
      [
        @auth_context,
        "POST",
        path,
        Integer.to_string(timestamp),
        nonce,
        body_hash(body)
      ]
      |> Enum.join("\n")

    signature = Identity.sign(identity, message) |> Base.encode64()

    [
      {"x-arc-public-key", Identity.encode_public_key(identity)},
      {"x-arc-timestamp", Integer.to_string(timestamp)},
      {"x-arc-nonce", nonce},
      {"x-arc-signature", signature}
    ]
  end

  defp session_headers(session_id, protocol_version) do
    [
      {"mcp-session-id", session_id},
      {"mcp-protocol-version", protocol_version}
    ]
  end

  defp http_request(server, method, headers, body) do
    {:ok, socket} =
      :gen_tcp.connect({127, 0, 0, 1}, HTTPServer.port(server), [
        :binary,
        active: false,
        packet: :raw
      ])

    request = [
      method,
      " /mcp HTTP/1.1\r\n",
      "Host: localhost\r\n",
      "Content-Type: application/json\r\n",
      Enum.map(headers, fn {key, value} -> [header_name(key), ": ", value, "\r\n"] end),
      "Content-Length: ",
      Integer.to_string(byte_size(body)),
      "\r\n\r\n",
      body
    ]

    :ok = :gen_tcp.send(socket, request)
    {:ok, response} = recv_until_close(socket, "")
    :gen_tcp.close(socket)
    parse_response(response)
  end

  defp recv_until_close(socket, acc) do
    case :gen_tcp.recv(socket, 0, 2_000) do
      {:ok, chunk} -> recv_until_close(socket, acc <> chunk)
      {:error, :closed} -> {:ok, acc}
      {:error, reason} -> {:error, reason}
    end
  end

  defp recv_until(socket, needle, timeout_ms) do
    deadline = System.monotonic_time(:millisecond) + timeout_ms
    recv_until(socket, needle, "", deadline)
  end

  defp recv_until(socket, needle, acc, deadline) do
    if String.contains?(acc, needle) do
      {:ok, acc}
    else
      remaining = deadline - System.monotonic_time(:millisecond)

      if remaining <= 0 do
        {:error, :timeout}
      else
        case :gen_tcp.recv(socket, 0, remaining) do
          {:ok, chunk} -> recv_until(socket, needle, acc <> chunk, deadline)
          {:error, reason} -> {:error, reason}
        end
      end
    end
  end

  defp parse_response(response) do
    [head, body] = String.split(response, "\r\n\r\n", parts: 2)
    [status_line | header_lines] = String.split(head, "\r\n")
    [<<"HTTP/1.1">>, status, _reason] = String.split(status_line, " ", parts: 3)

    headers =
      Enum.reduce(header_lines, %{}, fn line, acc ->
        case String.split(line, ":", parts: 2) do
          [key, value] -> Map.put(acc, String.downcase(String.trim(key)), String.trim(value))
          _ -> acc
        end
      end)

    %{status: String.to_integer(status), headers: headers, body: body}
  end

  defp wait_until(fun, attempts \\ 20)

  defp wait_until(fun, attempts) when attempts > 0 do
    if fun.() do
      true
    else
      Process.sleep(50)
      wait_until(fun, attempts - 1)
    end
  end

  defp wait_until(_fun, 0), do: false

  defp encode_json(map) do
    map
    |> :json.encode()
    |> IO.iodata_to_binary()
  end

  defp decode_json(body), do: :json.decode(body)

  defp body_hash(body) do
    :crypto.hash(:sha256, body)
    |> Base.encode16(case: :lower)
  end

  defp header_name(key) do
    key
    |> to_string()
    |> String.split("-")
    |> Enum.map_join("-", &String.capitalize/1)
  end

  defp stop_process(pid) when is_pid(pid) do
    if Process.alive?(pid), do: GenServer.stop(pid, :normal)
  end

  defp stop_process(_pid), do: :ok

  defp detail_doc(provider_name, capability_id) do
    %{
      "provider" => %{"name" => provider_name, "short_name" => provider_name},
      "capability" => %{
        "id" => capability_id,
        "kind" => "service",
        "scheme" => "http",
        "title" => "HTTP Proxy",
        "summary" => "Proxy HTTP requests",
        "invocation" => %{"method" => "RAW", "path" => "/"}
      }
    }
  end
end
