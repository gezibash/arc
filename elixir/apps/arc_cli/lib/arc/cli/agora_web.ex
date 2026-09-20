defmodule Arc.CLI.AgoraWeb do
  @moduledoc """
  Loopback-only browser surface for an installed Agora board.

  The browser never receives an ARC seed.  This process owns the citizen agent,
  signs posts locally, and invokes the installed, provider-pinned capability.
  """

  use GenServer

  alias Arc.Data.Agent
  alias Arc.Data.CapabilityInvocation
  alias Arc.Data.Toolbox
  alias Arc.Identity

  @host "127.0.0.1"
  @read_timeout 5_000
  @max_head_bytes 16_384
  @max_body_bytes 32_768
  @max_connections 32
  @max_cache_entries 1_000

  @index_path Path.expand("../../../priv/agora/index.html", __DIR__)
  @css_path Path.expand("../../../priv/agora/app.css", __DIR__)
  @js_path Path.expand("../../../priv/agora/app.js", __DIR__)
  @external_resource @index_path
  @external_resource @css_path
  @external_resource @js_path
  @index File.read!(@index_path)
  @css File.read!(@css_path)
  @js File.read!(@js_path)

  @spec start_link(keyword()) :: GenServer.on_start()
  def start_link(opts), do: GenServer.start_link(__MODULE__, opts)

  @spec port(pid()) :: non_neg_integer()
  def port(server), do: GenServer.call(server, :port)

  @spec url(pid()) :: String.t()
  def url(server), do: GenServer.call(server, :url)

  @doc "Returns a one-use fragment token for the local opener. Do not print this value."
  @spec launch_url(pid()) :: String.t()
  def launch_url(server), do: GenServer.call(server, :launch_url)

  @impl GenServer
  def init(opts) do
    Process.flag(:trap_exit, true)
    identity = Keyword.fetch!(opts, :identity)
    tool = Keyword.fetch!(opts, :tool)
    relay = Keyword.get(opts, :relay)
    relay_pubkey = Keyword.get(opts, :relay_pubkey)
    requested_port = Keyword.get(opts, :port, 0)

    with :ok <- validate_tool(tool),
         {:ok, agent} <- start_agent(identity, relay, relay_pubkey) do
      case listen(requested_port) do
        {:ok, socket} ->
          {:ok, port} = :inet.port(socket)
          server = self()
          {:ok, _acceptor} = Task.start(fn -> accept_loop(socket, server) end)

          {:ok,
           %{
             identity: identity,
             tool: tool,
             agent: agent,
             relay: relay,
             relay_pubkey: relay_pubkey,
             socket: socket,
             port: port,
             launch_token: token(),
             launch_used?: false,
             session_token: nil,
             cookie_name:
               "arc_agora_" <> Base.url_encode64(:crypto.strong_rand_bytes(9), padding: false),
             posts: %{},
             connections: MapSet.new()
           }}

        {:error, reason} ->
          stop_agent(identity, agent)
          {:stop, reason}
      end
    else
      {:error, reason} -> {:stop, reason}
    end
  rescue
    _ -> {:stop, :agora_start_failed}
  catch
    :exit, _ -> {:stop, :agora_start_failed}
  end

  @impl GenServer
  def terminate(_reason, state) do
    safe_close(state[:socket])

    stop_agent(state[:identity], state[:agent])

    :ok
  end

  @impl GenServer
  def handle_call(:port, _from, state), do: {:reply, state.port, state}
  def handle_call(:url, _from, state), do: {:reply, base_url(state), state}

  def handle_call(:launch_url, _from, state),
    do: {:reply, base_url(state) <> "#token=" <> state.launch_token, state}

  def handle_call({:request, request}, _from, state) do
    {response, state} = route(request, state)
    {:reply, response, state}
  end

  def handle_call({:accept, socket}, _from, state) do
    if MapSet.size(state.connections) >= @max_connections do
      safe_close(socket)
      {:reply, :busy, state}
    else
      server = self()

      {:ok, task} =
        Task.start(fn ->
          try do
            handle_socket(socket, server)
          after
            send(server, {:connection_done, self()})
          end
        end)

      {:reply, :ok, %{state | connections: MapSet.put(state.connections, task)}}
    end
  end

  @impl GenServer
  def handle_info({:connection_done, task}, state) do
    {:noreply, %{state | connections: MapSet.delete(state.connections, task)}}
  end

  @impl GenServer
  def format_status(status) when is_map(status) do
    status
    |> Map.put(:state, :redacted)
    |> Map.put(:message, :redacted)
    |> Map.put(:reason, :redacted)
    |> Map.put(:log, [])
  end

  defp validate_tool(tool) when is_map(tool) do
    capability = tool["capability"] || %{}
    version = get_in(Toolbox.cli_interface(capability) || %{}, ["version"])

    if Arc.Data.Agora.enabled?(capability) and is_integer(version) and
         version in 4..Arc.Data.InterfaceManifest.max_cli_version() and
         is_binary(Arc.Data.Agora.board(tool)) do
      :ok
    else
      {:error, :invalid_agora_tool}
    end
  end

  defp validate_tool(_), do: {:error, :invalid_agora_tool}

  defp start_agent(%Identity{} = identity, relay, relay_pubkey) do
    case Agent.start_link(identity) do
      {:ok, agent} ->
        case initialize_agent(identity, agent, relay, relay_pubkey) do
          :ok ->
            {:ok, agent}

          error ->
            stop_agent(identity, agent)
            error
        end

      error ->
        error
    end
  end

  defp initialize_agent(identity, agent, relay, relay_pubkey) do
    with :ok <- unlink_and_publish(agent),
         do: acquire_relay(identity, agent, relay, relay_pubkey)
  rescue
    _ -> {:error, :agora_agent_start_failed}
  catch
    :exit, _ -> {:error, :agora_agent_start_failed}
  end

  defp unlink_and_publish(agent) do
    Process.unlink(agent)
    Agent.publish(agent)
  end

  defp acquire_relay(_identity, _agent, nil, _pin), do: :ok

  defp acquire_relay(identity, agent, {host, port}, pin) do
    with :ok <- Arc.Net.acquire_relay(host, port, identity, pin, owner_pid: agent),
         :ok <- Agent.publish_relay(agent) do
      :ok
    else
      error ->
        _ = Arc.Net.release_relay(identity.public_key, agent)
        error
    end
  end

  defp acquire_relay(_identity, _agent, _relay, _pin), do: {:error, :invalid_relay}

  defp listen(port) when is_integer(port) and port >= 0 and port <= 65_535 do
    :gen_tcp.listen(port,
      mode: :binary,
      packet: :raw,
      active: false,
      reuseaddr: true,
      ip: {127, 0, 0, 1}
    )
  end

  defp listen(_), do: {:error, :invalid_port}

  defp accept_loop(socket, server) do
    case :gen_tcp.accept(socket) do
      {:ok, client} ->
        # A provider call can occupy the server while another browser request
        # arrives. Keep the acceptor alive until that call finishes.
        _ = GenServer.call(server, {:accept, client}, :infinity)
        accept_loop(socket, server)

      {:error, :closed} ->
        :ok

      {:error, _} ->
        :ok
    end
  catch
    :exit, _ -> :ok
  end

  defp handle_socket(socket, server) do
    response =
      case read_request(socket) do
        {:ok, request} -> GenServer.call(server, {:request, request}, 30_000)
        {:error, status, message} -> response(status, %{"error" => message})
      end

    send_response(socket, response)
  rescue
    _ -> send_response(socket, response(400, %{"error" => "invalid request"}))
  catch
    :exit, _ -> send_response(socket, response(503, %{"error" => "Agora is unavailable"}))
  after
    safe_close(socket)
  end

  defp route(request, state) do
    with :ok <- valid_host(request.headers, state.port),
         :ok <- valid_request_origin(request) do
      dispatch(request, state)
    else
      {:error, status, message} -> {response(status, %{"error" => message}), state}
    end
  end

  defp dispatch(%{method: "GET", path: "/"}, state),
    do: {asset(@index, "text/html; charset=utf-8"), state}

  defp dispatch(%{method: "GET", path: "/app.css"}, state),
    do: {asset(@css, "text/css; charset=utf-8"), state}

  defp dispatch(%{method: "GET", path: "/app.js"}, state),
    do: {asset(@js, "application/javascript; charset=utf-8"), state}

  defp dispatch(%{method: "POST", path: "/api/session", body: body, headers: headers}, state) do
    with :ok <- json_content_type(headers),
         {:ok, %{"token" => token}} <- json_object(body),
         true <- is_binary(token) and secure_equal?(token, state.launch_token),
         false <- state.launch_used? do
      session = token()

      {json(200, session_payload(state), cookie: {state.cookie_name, session}),
       %{state | launch_used?: true, session_token: session}}
    else
      _ -> {response(401, %{"error" => "open Agora from ARC"}), state}
    end
  end

  defp dispatch(%{method: "GET", path: "/api/session"} = request, state) do
    case authenticated?(request, state) do
      true -> {json(200, session_payload(state)), state}
      false -> {response(401, %{"error" => "open Agora from ARC"}), state}
    end
  end

  defp dispatch(%{method: "GET", path: "/api/feed", query: query} = request, state) do
    with true <- authenticated?(request, state),
         {:ok, args} <- pagination(query),
         {:ok, result} <- invoke(state, ["feed" | args]) do
      {json(200, result), state}
    else
      false -> {response(401, %{"error" => "open Agora from ARC"}), state}
      {:error, message} -> {response(400, %{"error" => message}), state}
    end
  end

  defp dispatch(%{method: "GET", path: "/api/thread", query: query} = request, state) do
    with true <- authenticated?(request, state),
         {:ok, id} <- post_id(query["id"]),
         {:ok, args} <- pagination(query),
         {:ok, result} <- invoke(state, ["thread", id | args]) do
      {json(200, result), state}
    else
      false -> {response(401, %{"error" => "open Agora from ARC"}), state}
      {:error, message} -> {response(400, %{"error" => message}), state}
    end
  end

  defp dispatch(
         %{method: "POST", path: "/api/posts", body: body, headers: headers} = request,
         state
       ) do
    with true <- authenticated?(request, state),
         :ok <- json_content_type(headers),
         {:ok, values} <- post_values(body),
         {:ok, result, state} <- invoke_post(state, values) do
      {json(200, result), state}
    else
      false -> {response(401, %{"error" => "open Agora from ARC"}), state}
      {:error, message, next_state} -> {response(400, %{"error" => message}), next_state}
      {:error, message} -> {response(400, %{"error" => message}), state}
    end
  end

  defp dispatch(%{path: "/api/" <> _rest}, state),
    do: {response(405, %{"error" => "method not allowed"}), state}

  defp dispatch(_request, state), do: {response(404, %{"error" => "not found"}), state}

  defp invoke_post(state, %{request_id: id, body: body, parent: parent}) do
    case cached_or_build(state, id, body, parent) do
      {:cached, result, next_state} ->
        {:ok, result, next_state}

      {:ok, built, next_state} ->
        case invoke_built(next_state, built) do
          {:ok, result} -> {:ok, result, put_in(next_state.posts[id].result, result)}
          {:error, message} -> {:error, message, next_state}
        end

      {:error, message} ->
        {:error, message, state}
    end
  end

  defp cached_or_build(state, id, body, parent) do
    case state.posts[id] do
      %{body: ^body, parent: ^parent, result: result} when is_map(result) ->
        {:cached, result, state}

      %{body: ^body, parent: ^parent, built: built} ->
        {:ok, built, state}

      nil ->
        if map_size(state.posts) >= @max_cache_entries do
          {:error, "too many pending post requests; restart Agora"}
        else
          case build_post_invocation(state, body, parent) do
            {:ok, built} ->
              entry = %{
                body: body,
                parent: parent,
                built: built,
                result: nil
              }

              {:ok, built, %{state | posts: Map.put(state.posts, id, entry)}}

            {:error, reason} ->
              {:error, error_message(reason)}
          end
        end

      _ ->
        {:error, "request_id cannot be reused for another post"}
    end
  end

  defp invoke(state, argv) do
    with {:ok, built} <- Toolbox.build_invocation(state.tool, argv, %{identity: state.identity}),
         do: invoke_built(state, built)
  rescue
    _ -> {:error, "could not prepare Agora request"}
  end

  # Browser text is structured data.  Do not run it through the CLI tokenizer:
  # a valid post may begin with `--` or contain newlines.
  defp build_post_invocation(state, body, parent) do
    operation = if parent == :null, do: "post", else: "reply"
    capability = state.tool["capability"] || %{}

    with {:ok, command} <- agora_command(capability, operation),
         {:ok, input, expected} <-
           Arc.Data.Agora.prepare(
             operation,
             %{"body" => body, "id" => if(parent == :null, do: nil, else: parent)},
             %{identity: state.identity, board: Arc.Data.Agora.board(state.tool)}
           ) do
      {:ok,
       %{
         input: input,
         agora: expected,
         command: command,
         invocation: Map.merge(capability["invocation"] || %{}, command["invoke"] || %{})
       }}
    else
      {:error, reason} -> {:error, error_message(reason)}
      _ -> {:error, "installed board does not support #{operation}"}
    end
  end

  defp agora_command(capability, operation) do
    command =
      Enum.find(Toolbox.cli_commands(capability), fn command ->
        command["path"] == [operation] and get_in(command, ["input", "source"]) == "agora" and
          get_in(command, ["input", "operation"]) == operation
      end)

    if is_map(command), do: {:ok, command}, else: {:error, :unsupported_agora_operation}
  end

  defp invoke_built(state, built) do
    with {:ok, reply} <-
           CapabilityInvocation.invoke(state.agent, state.tool, built.input,
             invocation_override: built.invocation
           ),
         {:ok, verified} <- Toolbox.finish_reply(reply, built),
         %{} = result <- :json.decode(verified.text) do
      {:ok, result}
    else
      {:error, reason} -> {:error, error_message(reason)}
      _ -> {:error, "Agora returned invalid data"}
    end
  rescue
    _ -> {:error, "Agora returned invalid data"}
  end

  defp post_values(body) do
    with {:ok, %{} = value} <- json_object(body),
         body when is_binary(body) <- value["body"],
         true <- String.valid?(body) and byte_size(body) <= 4096 and String.trim(body) != "",
         {:ok, parent} <- parent(value["parent"]),
         request_id when is_binary(request_id) <- value["request_id"],
         true <- byte_size(request_id) >= 16 and byte_size(request_id) <= 128 do
      {:ok, %{body: body, parent: parent, request_id: request_id}}
    else
      _ -> {:error, "invalid post"}
    end
  end

  defp parent(nil), do: {:ok, :null}
  defp parent(:null), do: {:ok, :null}
  defp parent(value), do: post_id(value)

  defp post_id(value) when is_binary(value) and byte_size(value) == 64 do
    if String.match?(value, ~r/\A[0-9a-f]+\z/),
      do: {:ok, value},
      else: {:error, "invalid post id"}
  end

  defp post_id(_), do: {:error, "invalid post id"}

  defp pagination(query) do
    with {:ok, cursor_value} <- cursor(query["after"]),
         {:ok, limit} <- limit(query["limit"]) do
      args = [] |> maybe_option("--after", cursor_value) |> maybe_option("--limit", limit)
      {:ok, args}
    end
  end

  defp cursor(nil), do: {:ok, nil}
  defp cursor(""), do: {:ok, nil}

  defp cursor(value) when is_binary(value) do
    case Integer.parse(value) do
      {number, ""} when number > 0 -> {:ok, value}
      _ -> {:error, "invalid cursor"}
    end
  end

  defp cursor(_), do: {:error, "invalid cursor"}

  defp limit(nil), do: {:ok, "20"}
  defp limit(""), do: {:ok, "20"}

  defp limit(value) when is_binary(value) do
    case Integer.parse(value) do
      {number, ""} when number in 1..50 -> {:ok, value}
      _ -> {:error, "invalid limit"}
    end
  end

  defp limit(_), do: {:error, "invalid limit"}
  defp maybe_option(args, _flag, nil), do: args
  defp maybe_option(args, flag, value), do: args ++ [flag, value]

  defp session_payload(state) do
    board = state.tool["provider"] || %{}
    mode = if state.relay, do: "relay", else: "local"

    %{
      "citizen" => %{
        "name" => Identity.name(state.identity),
        "public_key" => Identity.encode_public_key(state.identity)
      },
      "board" => %{
        "name" => board["name"] || board["short_name"] || "Agora",
        "public_key" => Arc.Data.Agora.board(state.tool)
      },
      "connection" => %{
        "mode" => mode,
        "label" => if(mode == "relay", do: "Relay", else: "Local")
      },
      "limits" => %{"body_bytes" => 4096}
    }
  end

  defp authenticated?(request, state) do
    with token when is_binary(token) <- cookie(request.headers["cookie"], state.cookie_name),
         session when is_binary(session) <- state.session_token do
      secure_equal?(token, session)
    else
      _ -> false
    end
  end

  defp valid_host(headers, port) do
    expected_port = Integer.to_string(port)

    if headers["host"] in ["127.0.0.1:" <> expected_port, "localhost:" <> expected_port],
      do: :ok,
      else: {:error, 400, "invalid host"}
  end

  defp valid_request_origin(%{headers: headers}) do
    origin = headers["origin"]
    fetch_site = headers["sec-fetch-site"]

    port = headers["host"] |> String.split(":") |> List.last()

    cond do
      origin != nil and origin not in ["http://127.0.0.1:" <> port, "http://localhost:" <> port] ->
        {:error, 403, "invalid origin"}

      fetch_site not in [nil, "same-origin", "none"] ->
        {:error, 403, "cross-site request denied"}

      true ->
        :ok
    end
  end

  defp asset(body, type), do: {:response, 200, security_headers([{"content-type", type}]), body}

  defp json(status, value, opts \\ []),
    do:
      {:response, status,
       security_headers(
         [{"content-type", "application/json; charset=utf-8"}] ++ cookie_header(opts)
       ), encode(value)}

  defp response(status, value), do: json(status, value)

  defp cookie_header(cookie: {name, value}),
    do: [{"set-cookie", name <> "=" <> value <> "; HttpOnly; SameSite=Strict; Path=/"}]

  defp cookie_header(_), do: []

  defp security_headers(headers),
    do:
      headers ++
        [
          {"cache-control", "no-store"},
          {"x-content-type-options", "nosniff"},
          {"content-security-policy",
           "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"},
          {"referrer-policy", "no-referrer"},
          {"connection", "close"}
        ]

  defp encode(value), do: value |> :json.encode() |> IO.iodata_to_binary()

  defp read_request(socket) do
    deadline = now_ms() + @read_timeout

    with {:ok, head, rest} <- recv_head(socket, "", deadline),
         {:ok, method, target, headers} <- parse_head(head),
         {:ok, path, query} <- parse_target(target),
         {:ok, length} <- content_length(headers),
         {:ok, body} <- recv_body(socket, rest, length, deadline) do
      {:ok, %{method: method, path: path, query: query, headers: headers, body: body}}
    else
      {:error, :timeout} -> {:error, 408, "request timeout"}
      {:error, :too_large} -> {:error, 413, "request too large"}
      {:error, _} -> {:error, 400, "invalid request"}
    end
  end

  defp recv_head(socket, acc, deadline) do
    case :binary.split(acc, "\r\n\r\n") do
      [head, rest] when byte_size(head) <= @max_head_bytes ->
        {:ok, head, rest}

      [_head, _rest] ->
        {:error, :too_large}

      [_] ->
        if byte_size(acc) > @max_head_bytes do
          {:error, :too_large}
        else
          case recv_before(socket, deadline),
            do: (
              {:ok, chunk} -> recv_head(socket, acc <> chunk, deadline)
              {:error, reason} -> {:error, reason}
            )
        end
    end
  end

  defp parse_head(head) do
    case String.split(head, "\r\n") do
      [line | lines] ->
        with [method, target, "HTTP/1.1"] <- String.split(line, " ", parts: 3),
             true <- method in ["GET", "POST"],
             {:ok, headers} <- parse_headers(lines) do
          {:ok, method, target, headers}
        else
          _ -> {:error, :invalid_request}
        end

      _ ->
        {:error, :invalid_request}
    end
  end

  defp parse_headers(lines) do
    Enum.reduce_while(lines, {:ok, %{}}, fn line, {:ok, headers} ->
      case String.split(line, ":", parts: 2) do
        [key, value] when key != "" ->
          key = String.downcase(String.trim(key))

          if Map.has_key?(headers, key),
            do: {:halt, {:error, :duplicate_header}},
            else: {:cont, {:ok, Map.put(headers, key, String.trim(value))}}

        _ ->
          {:halt, {:error, :invalid_header}}
      end
    end)
  end

  defp parse_target(target) do
    case URI.parse(target) do
      %URI{scheme: nil, host: nil, path: "/" <> _rest = path, query: query} ->
        {:ok, path, URI.decode_query(query || "")}

      _ ->
        {:error, :invalid_target}
    end
  rescue
    _ -> {:error, :invalid_target}
  end

  defp content_length(headers) do
    if Map.has_key?(headers, "transfer-encoding") do
      {:error, :invalid_transfer_encoding}
    else
      case headers["content-length"] || "0" do
        value ->
          case Integer.parse(value),
            do: (
              {length, ""} when length >= 0 and length <= @max_body_bytes -> {:ok, length}
              _ -> {:error, :too_large}
            )
      end
    end
  end

  defp recv_body(_socket, _rest, 0, _deadline), do: {:ok, <<>>}

  defp recv_body(_socket, rest, length, _deadline) when byte_size(rest) >= length,
    do: {:ok, binary_part(rest, 0, length)}

  defp recv_body(socket, rest, length, deadline) do
    case recv_before(socket, deadline),
      do: (
        {:ok, chunk} when byte_size(rest) + byte_size(chunk) <= @max_body_bytes ->
          recv_body(socket, rest <> chunk, length, deadline)

        {:ok, _} ->
          {:error, :too_large}

        {:error, reason} ->
          {:error, reason}
      )
  end

  defp recv_before(socket, deadline) do
    remaining = deadline - now_ms()
    if remaining > 0, do: :gen_tcp.recv(socket, 0, remaining), else: {:error, :timeout}
  end

  defp json_object(body) do
    case :json.decode(body) do
      %{} = value -> {:ok, value}
      _ -> {:error, :invalid_json}
    end
  rescue
    _ -> {:error, :invalid_json}
  end

  defp json_content_type(headers) do
    case headers["content-type"] do
      value when is_binary(value) ->
        if String.starts_with?(String.downcase(value), "application/json"),
          do: :ok,
          else: {:error, "expected application/json"}

      _ ->
        {:error, "expected application/json"}
    end
  end

  defp send_response(socket, {:response, status, headers, body}) do
    headers = [{"content-length", Integer.to_string(byte_size(body))} | headers]

    :gen_tcp.send(socket, [
      "HTTP/1.1 ",
      Integer.to_string(status),
      " ",
      reason(status),
      "\r\n",
      Enum.map(headers, fn {k, v} -> [k, ": ", v, "\r\n"] end),
      "\r\n",
      body
    ])
  end

  defp reason(200), do: "OK"
  defp reason(400), do: "Bad Request"
  defp reason(401), do: "Unauthorized"
  defp reason(403), do: "Forbidden"
  defp reason(404), do: "Not Found"
  defp reason(405), do: "Method Not Allowed"
  defp reason(408), do: "Request Timeout"
  defp reason(413), do: "Payload Too Large"
  defp reason(_), do: "Error"

  defp stop_agent(%Identity{} = identity, agent) when is_pid(agent) do
    if Process.alive?(agent), do: GenServer.stop(agent, :normal)
    Arc.Net.release_relay(identity.public_key, agent)
  catch
    # A dead owner also releases its lease through the manager's monitor. Keep
    # manager failure out of diagnostics containing the startup identity.
    :exit, _ -> {:error, :relay_release_failed}
  end

  defp stop_agent(_, _), do: :ok

  defp cookie(nil, _name), do: nil

  defp cookie(header, name),
    do:
      header
      |> String.split(";")
      |> Enum.find_value(fn part ->
        case String.split(String.trim(part), "=", parts: 2) do
          [^name, value] -> value
          _ -> nil
        end
      end)

  defp token, do: Base.url_encode64(:crypto.strong_rand_bytes(32), padding: false)

  defp secure_equal?(left, right) when is_binary(left) and is_binary(right),
    do:
      byte_size(left) == byte_size(right) and
        :crypto.hash(:sha256, left) == :crypto.hash(:sha256, right)

  defp secure_equal?(_, _), do: false
  defp base_url(state), do: "http://#{@host}:#{state.port}/"
  defp now_ms, do: System.monotonic_time(:millisecond)
  defp safe_close(nil), do: :ok
  defp safe_close(socket), do: :gen_tcp.close(socket)
  defp error_message({:invalid_arguments, value}) when is_binary(value), do: value
  defp error_message({:remote, _code, value}) when is_binary(value), do: value
  defp error_message(:timeout), do: "Agora did not respond"
  defp error_message(_), do: "Agora request failed"
end
