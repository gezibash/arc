defmodule Arc.MCP.HTTPServer do
  @moduledoc """
  Streamable HTTP transport for ARC's identity-scoped MCP surface.
  """

  use GenServer

  alias Arc.Data.Agent
  alias Arc.Identity
  alias Arc.Identity.KeyStore
  alias Arc.MCP
  alias Arc.MCP.Server

  @auth_context "ARC-MCP-AUTH-V1"
  @auth_skew_ms 60_000
  @default_host "127.0.0.1"
  @default_path "/mcp"
  @default_port 5004
  @read_timeout 5_000
  @keepalive_ms 15_000

  @spec start_link(keyword()) :: GenServer.on_start()
  def start_link(opts) do
    GenServer.start_link(__MODULE__, opts)
  end

  @spec port(pid()) :: non_neg_integer()
  def port(server) do
    GenServer.call(server, :port)
  end

  @spec url(pid()) :: String.t()
  def url(server) do
    GenServer.call(server, :url)
  end

  @impl GenServer
  def init(opts) do
    task = Keyword.fetch!(opts, :task)
    host = Keyword.get(opts, :host, @default_host)
    path = Keyword.get(opts, :path, @default_path)
    port = Keyword.get(opts, :port, @default_port)
    poll_ms = Keyword.get(opts, :poll_ms, 500)
    relay_addr = Keyword.get(opts, :relay)
    relay_pubkey_pin = Keyword.get(opts, :relay_pubkey)
    relay_idle_timeout_ms = Keyword.get(opts, :relay_idle_timeout_ms)
    registry_opts = Keyword.get(opts, :registry_opts, [])
    ip = parse_host!(host)

    {:ok, session_sup} = DynamicSupervisor.start_link(strategy: :one_for_one)

    {:ok, listen_sock} =
      :gen_tcp.listen(port, [
        :binary,
        packet: :raw,
        active: false,
        reuseaddr: true,
        ip: ip
      ])

    {:ok, actual_port} = :inet.port(listen_sock)
    server = self()
    {:ok, acceptor} = Task.start_link(fn -> accept_loop(listen_sock, server) end)

    {:ok,
     %{
       task: task,
       host: host,
       path: path,
       port: actual_port,
       poll_ms: poll_ms,
       relay_addr: relay_addr,
       relay_pubkey_pin: relay_pubkey_pin,
       relay_idle_timeout_ms: relay_idle_timeout_ms,
       registry_opts: registry_opts,
       listen_sock: listen_sock,
       acceptor: acceptor,
       session_sup: session_sup,
       sessions: %{}
     }}
  end

  @impl GenServer
  def terminate(_reason, state) do
    safe_close_socket(state.listen_sock)

    Enum.each(state.sessions, fn {_session_id, session} ->
      close_stream(session)
      stop_session(session.pid)
      release_session_transport(session)
      stop_agent(session.agent_pid)
    end)

    :ok
  end

  @impl GenServer
  def handle_call(:port, _from, state), do: {:reply, state.port, state}

  def handle_call(:url, _from, state) do
    {:reply, "http://#{state.host}:#{state.port}#{state.path}", state}
  end

  def handle_call({:http_request, request, connection_pid}, _from, state) do
    {response, state} = route_request(request, connection_pid, state)
    {:reply, response, state}
  end

  @impl GenServer
  def handle_cast({:stream_closed, session_id, pid}, state) do
    state =
      update_in(state.sessions, fn sessions ->
        case Map.get(sessions, session_id) do
          %{stream_pid: ^pid} = session ->
            Server.unsubscribe(session.pid, pid)
            Map.put(sessions, session_id, %{session | stream_pid: nil})

          _ ->
            sessions
        end
      end)

    {:noreply, state}
  end

  defp accept_loop(listen_sock, server) do
    case :gen_tcp.accept(listen_sock) do
      {:ok, socket} ->
        Task.start(fn -> handle_socket(socket, server) end)
        accept_loop(listen_sock, server)

      {:error, :closed} ->
        :ok

      {:error, _reason} ->
        :ok
    end
  end

  defp handle_socket(socket, server) do
    case read_request(socket) do
      {:ok, request} ->
        case GenServer.call(server, {:http_request, request, self()}, 30_000) do
          {:response, status, headers, body} ->
            send_response(socket, status, headers, body)

          {:sse, headers, session_id} ->
            send_sse_headers(socket, headers)
            :ok = :gen_tcp.send(socket, ": connected\n\n")
            sse_loop(socket, server, session_id)
        end

      {:error, status, body} ->
        send_response(socket, status, [{"content-type", "text/plain"}], body)
    end
  after
    safe_close_socket(socket)
  end

  defp sse_loop(socket, server, session_id) do
    receive do
      {:arc_mcp, message} ->
        send_sse_message(socket, message)
        sse_loop(socket, server, session_id)

      {:close, _reason} ->
        GenServer.cast(server, {:stream_closed, session_id, self()})
        :ok
    after
      @keepalive_ms ->
        case :gen_tcp.send(socket, ": keepalive\n\n") do
          :ok ->
            sse_loop(socket, server, session_id)

          {:error, _} ->
            GenServer.cast(server, {:stream_closed, session_id, self()})
            :ok
        end
    end
  end

  defp route_request(request, connection_pid, state) do
    with :ok <- validate_path(request.path, state.path),
         :ok <- validate_origin(request.headers) do
      do_route_request(request, connection_pid, state)
    else
      {:error, status, message} ->
        {{:response, status, [{"content-type", "text/plain"}], message}, state}
    end
  end

  defp do_route_request(%{method: "POST"} = request, _connection_pid, state) do
    case decode_json_body(request.body) do
      {:ok, json} ->
        if initialize_request?(json) do
          create_session_and_initialize(request, json, state)
        else
          handle_post_request(request, json, state)
        end

      {:error, :parse_error} ->
        {{:response, 400, [{"content-type", "application/json"}], encode_json(parse_error())},
         state}
    end
  end

  defp do_route_request(%{method: "GET", headers: headers}, connection_pid, state) do
    with true <- accepts_sse?(headers),
         {:ok, session_id} <- fetch_session_id(headers),
         {:ok, session} <- fetch_session(state, session_id),
         state <- replace_stream(state, session_id, connection_pid),
         :ok <- Server.subscribe(session.pid, connection_pid) do
      headers = [
        {"content-type", "text/event-stream"},
        {"cache-control", "no-cache"},
        {"connection", "keep-alive"},
        {"mcp-session-id", session_id},
        {"mcp-protocol-version", session.protocol_version}
      ]

      {{:sse, headers, session_id}, state}
    else
      false ->
        {{:response, 406, [{"content-type", "text/plain"}], "expected Accept: text/event-stream"},
         state}

      {:error, status, message} ->
        {{:response, status, [{"content-type", "text/plain"}], message}, state}
    end
  end

  defp do_route_request(%{method: "DELETE", headers: headers}, _connection_pid, state) do
    with {:ok, session_id} <- fetch_session_id(headers),
         {:ok, session} <- fetch_session(state, session_id) do
      close_stream(session)
      stop_session(session.pid)
      release_session_transport(session)
      stop_agent(session.agent_pid)
      state = %{state | sessions: Map.delete(state.sessions, session_id)}
      {{:response, 204, [{"mcp-session-id", session_id}], ""}, state}
    else
      {:error, status, message} ->
        {{:response, status, [{"content-type", "text/plain"}], message}, state}
    end
  end

  defp do_route_request(%{method: method}, _connection_pid, state) do
    {{:response, 405, [{"content-type", "text/plain"}], "unsupported method #{method}"}, state}
  end

  defp create_session_and_initialize(http_request, request, state) do
    session_id = new_session_id()

    with {:ok, identity} <- authenticate_initialize(http_request),
         {:ok, agent_pid} <- start_session_agent(identity, state) do
      case start_session_server(identity, agent_pid, state) do
        {:ok, session_pid} ->
          response = Server.request(session_pid, request)

          protocol_version =
            get_in(response, ["result", "protocolVersion"]) || MCP.protocol_version()

          session = %{
            pid: session_pid,
            agent_pid: agent_pid,
            owner_name: Identity.name(identity),
            owner_public_key: identity.public_key,
            protocol_version: protocol_version,
            stream_pid: nil
          }

          state = %{state | sessions: Map.put(state.sessions, session_id, session)}

          headers = [
            {"content-type", "application/json"},
            {"mcp-session-id", session_id},
            {"mcp-protocol-version", protocol_version}
          ]

          {{:response, 200, headers, encode_json(response)}, state}

        {:error, reason} ->
          release_agent_transport(identity.public_key, agent_pid)
          stop_agent(agent_pid)

          {{:response, 500, [{"content-type", "text/plain"}],
            "failed to initialize session: #{inspect(reason)}"}, state}
      end
    else
      {:error, :unauthorized} ->
        {{:response, 401, [{"content-type", "text/plain"}], "invalid ARC auth"}, state}

      {:error, :identity_not_found} ->
        {{:response, 403, [{"content-type", "text/plain"}],
          "ARC key is not present in local KeyStore"}, state}

      {:error, :identity_in_use} ->
        {{:response, 409, [{"content-type", "text/plain"}],
          "ARC key is already active in this runtime"}, state}

      {:error, reason} ->
        {{:response, 500, [{"content-type", "text/plain"}],
          "failed to initialize session: #{inspect(reason)}"}, state}
    end
  end

  defp handle_post_request(request, json, state) do
    with {:ok, session_id} <- fetch_session_id(request.headers),
         {:ok, session} <- fetch_session(state, session_id),
         :ok <- validate_protocol_header(request.headers, session.protocol_version) do
      response = Server.request(session.pid, json)

      headers = [
        {"content-type", "application/json"},
        {"mcp-session-id", session_id},
        {"mcp-protocol-version", session.protocol_version}
      ]

      case response do
        nil -> {{:response, 202, headers, ""}, state}
        _ -> {{:response, 200, headers, encode_json(response)}, state}
      end
    else
      {:error, status, message} ->
        {{:response, status, [{"content-type", "text/plain"}], message}, state}
    end
  end

  defp replace_stream(state, session_id, new_stream_pid) do
    update_in(state.sessions, fn sessions ->
      case Map.get(sessions, session_id) do
        nil ->
          sessions

        %{stream_pid: existing} = session when is_pid(existing) and existing != new_stream_pid ->
          send(existing, {:close, :replaced})
          Map.put(sessions, session_id, %{session | stream_pid: new_stream_pid})

        %{stream_pid: _existing} = session ->
          Map.put(sessions, session_id, %{session | stream_pid: new_stream_pid})
      end
    end)
  end

  defp fetch_session(%{sessions: sessions}, session_id) do
    case Map.get(sessions, session_id) do
      nil -> {:error, 404, "unknown MCP session"}
      session -> {:ok, session}
    end
  end

  defp fetch_session_id(headers) do
    case Map.get(headers, "mcp-session-id") do
      nil -> {:error, 400, "missing Mcp-Session-Id header"}
      "" -> {:error, 400, "missing Mcp-Session-Id header"}
      session_id -> {:ok, session_id}
    end
  end

  defp validate_protocol_header(headers, negotiated_version) do
    case Map.get(headers, "mcp-protocol-version") do
      nil ->
        :ok

      ^negotiated_version ->
        :ok

      version ->
        if version in MCP.supported_protocol_versions() do
          {:error, 400, "protocol version mismatch for session"}
        else
          {:error, 400, "unsupported MCP-Protocol-Version"}
        end
    end
  end

  defp validate_path(path, expected) when path == expected, do: :ok
  defp validate_path(_path, _expected), do: {:error, 404, "not found"}

  defp validate_origin(headers) do
    case Map.get(headers, "origin") do
      nil -> :ok
      "" -> :ok
      origin -> if local_origin?(origin), do: :ok, else: {:error, 403, "forbidden origin"}
    end
  end

  defp local_origin?(origin) do
    uri = URI.parse(origin)
    uri.scheme in ["http", "https"] and uri.host in ["localhost", "127.0.0.1", "::1", "[::1]"]
  end

  defp initialize_request?(%{"method" => "initialize"}), do: true
  defp initialize_request?(_), do: false

  defp authenticate_initialize(request) do
    with {:ok, public_key} <- fetch_pubkey(request.headers),
         {:ok, timestamp} <- fetch_timestamp(request.headers),
         {:ok, nonce} <- fetch_nonce(request.headers),
         {:ok, signature} <- fetch_signature(request.headers),
         :ok <- validate_timestamp(timestamp),
         {:ok, identity} <- fetch_identity(public_key),
         true <- Identity.verify(public_key, auth_message(request, timestamp, nonce), signature) do
      {:ok, identity}
    else
      {:error, :not_found} -> {:error, :identity_not_found}
      false -> {:error, :unauthorized}
      _ -> {:error, :unauthorized}
    end
  end

  defp accepts_sse?(headers) do
    headers
    |> Map.get("accept", "")
    |> String.downcase()
    |> String.contains?("text/event-stream")
  end

  defp decode_json_body(body) when is_binary(body) do
    case :json.decode(body) do
      %{} = json -> {:ok, json}
      _ -> {:error, :parse_error}
    end
  rescue
    _ -> {:error, :parse_error}
  end

  defp parse_error do
    %{
      "jsonrpc" => "2.0",
      "id" => nil,
      "error" => %{"code" => -32_700, "message" => "parse error"}
    }
  end

  defp encode_json(map) do
    map
    |> :json.encode()
    |> IO.iodata_to_binary()
  end

  defp start_session_server(identity, agent_pid, state) do
    DynamicSupervisor.start_child(state.session_sup, {
      Server,
      [
        task: state.task,
        owner: identity.public_key,
        agent: agent_pid,
        poll_ms: state.poll_ms,
        registry_opts: state.registry_opts
      ]
    })
  end

  defp start_session_agent(identity, state) do
    case Agent.start_link(identity) do
      {:ok, agent_pid} ->
        Process.unlink(agent_pid)
        :ok = Agent.publish(agent_pid)
        maybe_acquire_relay(identity, agent_pid, state)
        {:ok, agent_pid}

      {:error, {:already_registered, _public_key}} ->
        {:error, :identity_in_use}

      {:error, reason} ->
        {:error, reason}
    end
  end

  defp maybe_acquire_relay(identity, agent_pid, state) do
    case state.relay_addr do
      {host, port} ->
        _ =
          Arc.Net.acquire_relay(
            host,
            port,
            identity,
            state.relay_pubkey_pin,
            relay_acquire_opts(agent_pid, state)
          )

        :ok

      _ ->
        :ok
    end
  end

  defp relay_acquire_opts(agent_pid, state) do
    opts = [owner_pid: agent_pid]

    case state.relay_idle_timeout_ms do
      timeout when is_integer(timeout) and timeout >= 0 ->
        Keyword.put(opts, :idle_timeout_ms, timeout)

      _ ->
        opts
    end
  end

  defp release_session_transport(session) do
    release_agent_transport(session.owner_public_key, session.agent_pid)
  end

  defp release_agent_transport(owner_public_key, agent_pid)
       when is_binary(owner_public_key) and is_pid(agent_pid) do
    _ = Arc.Net.release_relay(owner_public_key, agent_pid)
    :ok
  end

  defp release_agent_transport(_owner_public_key, _agent_pid), do: :ok

  defp fetch_identity(public_key) do
    case KeyStore.get_by_public_key(public_key) do
      {:ok, identity} -> {:ok, identity}
      {:error, :not_found} -> {:error, :not_found}
    end
  end

  defp fetch_pubkey(headers) do
    headers
    |> Map.get("x-arc-public-key")
    |> decode_binary(32)
  end

  defp fetch_signature(headers) do
    headers
    |> Map.get("x-arc-signature")
    |> decode_binary(64)
  end

  defp fetch_timestamp(headers) do
    case Map.get(headers, "x-arc-timestamp") do
      nil ->
        {:error, :missing_timestamp}

      value ->
        case Integer.parse(value) do
          {timestamp, ""} -> {:ok, timestamp}
          _ -> {:error, :invalid_timestamp}
        end
    end
  end

  defp fetch_nonce(headers) do
    case Map.get(headers, "x-arc-nonce") do
      nonce when is_binary(nonce) and nonce != "" -> {:ok, nonce}
      _ -> {:error, :missing_nonce}
    end
  end

  defp decode_binary(value, expected_size) when is_binary(value) do
    value = String.trim(value)

    case Base.decode16(String.trim_leading(value, "0x"), case: :mixed) do
      {:ok, decoded} when byte_size(decoded) == expected_size ->
        {:ok, decoded}

      _ ->
        case Base.decode64(value) do
          {:ok, decoded} when byte_size(decoded) == expected_size -> {:ok, decoded}
          _ -> {:error, :invalid_header}
        end
    end
  end

  defp decode_binary(_value, _expected_size), do: {:error, :invalid_header}

  defp validate_timestamp(timestamp_ms) when is_integer(timestamp_ms) do
    now = System.system_time(:millisecond)

    if abs(now - timestamp_ms) <= @auth_skew_ms do
      :ok
    else
      {:error, :stale_timestamp}
    end
  end

  defp auth_message(request, timestamp_ms, nonce) do
    [
      @auth_context,
      request.method,
      request.path,
      Integer.to_string(timestamp_ms),
      nonce,
      body_hash(request.body)
    ]
    |> Enum.join("\n")
  end

  defp body_hash(body) when is_binary(body) do
    :crypto.hash(:sha256, body)
    |> Base.encode16(case: :lower)
  end

  defp parse_host!("localhost"), do: {127, 0, 0, 1}
  defp parse_host!("127.0.0.1"), do: {127, 0, 0, 1}
  defp parse_host!("0.0.0.0"), do: {0, 0, 0, 0}

  defp parse_host!(host) when is_binary(host) do
    case :inet.parse_address(String.to_charlist(host)) do
      {:ok, ip} -> ip
      {:error, _} -> raise ArgumentError, "invalid MCP host #{inspect(host)}"
    end
  end

  defp read_request(socket) do
    with {:ok, head, rest} <- recv_until_headers(socket, ""),
         {:ok, request_line, header_lines} <- split_head(head),
         {:ok, method, path} <- parse_request_line(request_line),
         headers <- parse_headers(header_lines),
         {:ok, body} <- recv_body(socket, rest, content_length(headers)) do
      {:ok, %{method: method, path: path, headers: headers, body: body}}
    else
      {:error, :timeout} -> {:error, 408, "request timeout"}
      {:error, _reason} -> {:error, 400, "invalid request"}
    end
  end

  defp recv_until_headers(socket, acc) do
    case String.split(acc, "\r\n\r\n", parts: 2) do
      [head, rest] ->
        {:ok, head, rest}

      [_] ->
        case :gen_tcp.recv(socket, 0, @read_timeout) do
          {:ok, chunk} -> recv_until_headers(socket, acc <> chunk)
          {:error, reason} -> {:error, reason}
        end
    end
  end

  defp split_head(head) do
    case String.split(head, "\r\n") do
      [request_line | header_lines] -> {:ok, request_line, header_lines}
      _ -> {:error, :invalid_request}
    end
  end

  defp parse_request_line(line) do
    case String.split(line, " ", parts: 3) do
      [method, path, <<"HTTP/", _::binary>>] -> {:ok, method, path}
      _ -> {:error, :invalid_request_line}
    end
  end

  defp parse_headers(lines) do
    Enum.reduce(lines, %{}, fn line, acc ->
      case String.split(line, ":", parts: 2) do
        [key, value] ->
          Map.put(acc, String.downcase(String.trim(key)), String.trim(value))

        _ ->
          acc
      end
    end)
  end

  defp content_length(headers) do
    case Integer.parse(Map.get(headers, "content-length", "0")) do
      {length, ""} when length >= 0 -> length
      _ -> 0
    end
  end

  defp recv_body(_socket, rest, 0), do: {:ok, binary_part(rest, 0, 0)}

  defp recv_body(_socket, rest, content_length) when byte_size(rest) >= content_length do
    {:ok, binary_part(rest, 0, content_length)}
  end

  defp recv_body(socket, rest, content_length) do
    case :gen_tcp.recv(socket, 0, @read_timeout) do
      {:ok, chunk} -> recv_body(socket, rest <> chunk, content_length)
      {:error, reason} -> {:error, reason}
    end
  end

  defp send_response(socket, status, headers, body) do
    body = body || ""
    headers = put_header(headers, "content-length", Integer.to_string(byte_size(body)))

    response = [
      "HTTP/1.1 ",
      Integer.to_string(status),
      " ",
      reason_phrase(status),
      "\r\n",
      Enum.map(headers, fn {key, value} -> [header_name(key), ": ", to_string(value), "\r\n"] end),
      "\r\n",
      body
    ]

    :gen_tcp.send(socket, response)
  end

  defp send_sse_headers(socket, headers) do
    response = [
      "HTTP/1.1 200 OK\r\n",
      Enum.map(headers, fn {key, value} -> [header_name(key), ": ", to_string(value), "\r\n"] end),
      "\r\n"
    ]

    :gen_tcp.send(socket, response)
  end

  defp send_sse_message(socket, message) do
    payload = encode_json(message)
    :gen_tcp.send(socket, ["event: message\r\ndata: ", payload, "\r\n\r\n"])
  end

  defp header_name(key) do
    key
    |> to_string()
    |> String.split("-")
    |> Enum.map_join("-", &String.capitalize/1)
  end

  defp put_header(headers, key, value) do
    if Enum.any?(headers, fn {existing, _} -> String.downcase(to_string(existing)) == key end) do
      headers
    else
      headers ++ [{key, value}]
    end
  end

  defp new_session_id do
    :crypto.strong_rand_bytes(16)
    |> Base.url_encode64(padding: false)
  end

  defp stop_agent(pid) when is_pid(pid) do
    if Process.alive?(pid), do: GenServer.stop(pid, :normal)
  end

  defp stop_agent(_pid), do: :ok

  defp close_stream(%{stream_pid: pid}) when is_pid(pid),
    do: send(pid, {:close, :server_shutdown})

  defp close_stream(_session), do: :ok

  defp stop_session(pid) when is_pid(pid) do
    if Process.alive?(pid), do: GenServer.stop(pid, :normal)
  end

  defp safe_close_socket(nil), do: :ok
  defp safe_close_socket(socket), do: :gen_tcp.close(socket)

  defp reason_phrase(200), do: "OK"
  defp reason_phrase(202), do: "Accepted"
  defp reason_phrase(204), do: "No Content"
  defp reason_phrase(401), do: "Unauthorized"
  defp reason_phrase(400), do: "Bad Request"
  defp reason_phrase(403), do: "Forbidden"
  defp reason_phrase(404), do: "Not Found"
  defp reason_phrase(405), do: "Method Not Allowed"
  defp reason_phrase(406), do: "Not Acceptable"
  defp reason_phrase(408), do: "Request Timeout"
  defp reason_phrase(409), do: "Conflict"
  defp reason_phrase(500), do: "Internal Server Error"
  defp reason_phrase(status) when is_integer(status), do: "Error"
end
