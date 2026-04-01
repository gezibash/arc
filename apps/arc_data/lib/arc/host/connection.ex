defmodule Arc.Host.Connection do
  @moduledoc false

  use GenServer

  alias Arc.Control
  alias Arc.Data.Agent
  alias Arc.Data.CapabilityDiscovery
  alias Arc.Data.CapabilityInvocation
  alias Arc.Data.Frame
  alias Arc.Data.Toolbox
  alias Arc.Host.AgentPool
  alias Arc.Host.Protocol
  alias Arc.Host.Service
  alias Arc.Host.Token
  alias Arc.Identity

  @default_timeout_ms 10_000

  def start_link(opts) do
    GenServer.start_link(__MODULE__, opts)
  end

  @impl GenServer
  def init(opts) do
    Process.flag(:trap_exit, true)

    socket = Keyword.fetch!(opts, :socket)
    agent_pool = Keyword.fetch!(opts, :agent_pool)
    service = Keyword.fetch!(opts, :service)

    owner = self()
    {:ok, reader_pid} = Task.start_link(fn -> read_loop(socket, owner, "") end)
    reader_ref = Process.monitor(reader_pid)

    {:ok,
     %{
       socket: socket,
       agent_pool: agent_pool,
       service: service,
       reader: reader_pid,
       reader_ref: reader_ref,
       binding: nil,
       streams: %{}
     }}
  end

  @impl GenServer
  def handle_info({:socket_line, line}, state) do
    state =
      case decode_request(line) do
        {:ok, request} ->
          handle_request(request, state)

        {:error, reason} ->
          send_error(state.socket, nil, "invalid_request", inspect(reason))
          state
      end

    {:noreply, state}
  end

  def handle_info({:socket_closed, :closed}, state) do
    {:stop, :normal, state}
  end

  def handle_info({:socket_closed, reason}, state) do
    {:stop, {:socket_closed, reason}, state}
  end

  def handle_info({:DOWN, ref, :process, _pid, reason}, %{reader_ref: ref} = state) do
    {:stop, {:reader_down, reason}, state}
  end

  def handle_info({:DOWN, ref, :process, _pid, _reason}, state) do
    stream_id =
      Enum.find_value(state.streams, fn {app_session_id, stream} ->
        if stream.pump_ref == ref, do: app_session_id, else: nil
      end)

    {:noreply, maybe_drop_stream(state, stream_id)}
  end

  def handle_info({:host_stream_event, app_session_id, event}, state) do
    send_json(state.socket, event)
    {:noreply, maybe_drop_stream(state, final_event_stream_id(app_session_id, event))}
  end

  def handle_info(_msg, state), do: {:noreply, state}

  @impl GenServer
  def terminate(_reason, state) do
    close_streams(state)

    if is_pid(state.reader) do
      Process.exit(state.reader, :normal)
    end

    _ = :socket.close(state.socket)
    :ok
  end

  defp handle_request(%{"id" => id, "op" => "status"}, state) do
    result = Service.status(state.service)
    send_ok(state.socket, id, result)
    state
  end

  defp handle_request(%{"id" => id, "op" => "protocol.describe"}, state) do
    send_ok(state.socket, id, Protocol.describe())
    state
  end

  defp handle_request(%{"id" => id, "op" => "shutdown", "params" => params}, state) do
    with {:ok, _record} <- authenticate_admin_token(state, request_token(params)) do
      send_ok(state.socket, id, %{"stopping" => true})
      Service.shutdown(state.service)
      state
    else
      {:error, reason} ->
        send_error(state.socket, id, auth_error_code(reason), auth_error_message(reason))
        state
    end
  end

  defp handle_request(%{"id" => id, "op" => "token.issue", "params" => params}, state) do
    case Service.issue_token(
           state.service,
           request_token(params),
           identity: blank_to_nil(Map.get(params, "identity")),
           scopes: normalize_scopes_param(Map.get(params, "scopes")),
           ttl_seconds: Map.get(params, "ttl_seconds"),
           label: blank_to_nil(Map.get(params, "label"))
         ) do
      {:ok, issued} ->
        send_ok(state.socket, id, issued)
        state

      {:error, reason} ->
        send_error(
          state.socket,
          id,
          token_issue_error_code(reason),
          token_issue_error_message(reason)
        )

        state
    end
  end

  defp handle_request(%{"id" => id, "op" => "initialize", "params" => params}, state) do
    with true <- is_nil(state.binding) or {:error, :already_initialized},
         {:ok, token_record} <- Service.authenticate_token(state.service, request_token(params)),
         :ok <- ensure_delegated_token(token_record),
         :ok <- ensure_identity_matches(token_record, blank_to_nil(Map.get(params, "identity"))),
         {:ok, %{identity: identity, agent: agent}} <-
           AgentPool.acquire(state.agent_pool, token_record.identity) do
      send_ok(state.socket, id, %{
        "identity" => identity_document(identity),
        "token" => %{
          "id" => token_record.id,
          "label" => token_record.label,
          "scopes" => Token.scopes_list(token_record),
          "expires_at" =>
            if(token_record.expires_at,
              do: DateTime.to_iso8601(token_record.expires_at),
              else: nil
            )
        }
      })

      %{state | binding: %{identity: identity, agent: agent, token: token_record}}
    else
      {:error, :already_initialized} ->
        send_error(state.socket, id, "already_initialized", "connection is already initialized")
        state

      {:error, reason} ->
        send_error(
          state.socket,
          id,
          initialize_error_code(reason),
          initialize_error_message(reason)
        )

        state
    end
  end

  defp handle_request(request, %{binding: nil} = state) do
    send_error(
      state.socket,
      request["id"],
      "not_initialized",
      "call initialize with a delegated token first"
    )

    state
  end

  defp handle_request(%{"id" => id, "op" => "identity.current"}, state) do
    send_ok(state.socket, id, identity_document(state.binding.identity))
    state
  end

  defp handle_request(%{"id" => id, "op" => "resolve", "params" => params}, state) do
    with_scope(state, id, "resolve", fn ->
      query = Map.get(params, "query", "")

      case Control.resolve(query) do
        {:ok, entries} ->
          result =
            Enum.map(entries, fn entry ->
              %{
                "name" => entry.name,
                "short_name" => entry.short_name,
                "public_key" => encode_hex(entry.public_key),
                "has_key_exchange" => not is_nil(entry.x25519_public),
                "status" => Atom.to_string(entry.status)
              }
            end)

          send_ok(state.socket, id, %{"matches" => result})

        {:error, reason} ->
          send_error(state.socket, id, "resolve_failed", inspect(reason))
      end

      state
    end)
  end

  defp handle_request(%{"id" => id, "op" => "discover", "params" => params}, state) do
    with_scope(state, id, "discover", fn ->
      query = Map.get(params, "query", "")

      case CapabilityDiscovery.discover(state.binding.agent, query) do
        {:ok, result} ->
          send_ok(state.socket, id, normalize_discovery(result))

        {:error, reason} ->
          send_error(state.socket, id, "discover_failed", inspect(reason))
      end

      state
    end)
  end

  defp handle_request(%{"id" => id, "op" => "info", "params" => params}, state) do
    with_scope(state, id, "info", fn ->
      peer = Map.get(params, "peer")
      capability_id = Map.get(params, "capability")

      result =
        cond do
          !is_binary(peer) or peer == "" ->
            {:error, {"invalid_peer", "peer is required"}}

          is_binary(capability_id) and capability_id != "" ->
            CapabilityDiscovery.fetch_detail(state.binding.agent, peer, capability_id)

          true ->
            request_document(state.binding.agent, peer, "/info")
        end

      case result do
        {:ok, document} ->
          send_ok(state.socket, id, document)

        {:error, {:remote, code, message}} ->
          send_error(state.socket, id, code, message)

        {:error, {code, message}} ->
          send_error(state.socket, id, code, message)

        {:error, reason} ->
          send_error(state.socket, id, "info_failed", inspect(reason))
      end

      state
    end)
  end

  defp handle_request(%{"id" => id, "op" => "send", "params" => params}, state) do
    with_scope(state, id, "send", fn ->
      to = Map.get(params, "to")
      message = Map.get(params, "message", "")

      case send_message(state.binding.agent, to, message) do
        {:ok, request_id} ->
          send_ok(state.socket, id, %{"request_id" => encode_hex(request_id)})

        {:error, reason} ->
          send_error(state.socket, id, "send_failed", format_reason(reason))
      end

      state
    end)
  end

  defp handle_request(%{"id" => id, "op" => "call", "params" => params}, state) do
    with_scope(state, id, "call", fn ->
      peer = Map.get(params, "peer")
      capability_id = Map.get(params, "capability")
      input = Map.get(params, "input", "")
      timeout_ms = Map.get(params, "timeout_ms", @default_timeout_ms)

      result =
        with true <-
               (is_binary(peer) and peer != "") or {:error, {"invalid_peer", "peer is required"}},
             true <-
               (is_binary(capability_id) and capability_id != "") or
                 {:error, {"invalid_capability", "capability is required"}},
             {:ok, detail} <-
               CapabilityDiscovery.fetch_detail(state.binding.agent, peer, capability_id),
             {:ok, reply} <-
               CapabilityInvocation.invoke(state.binding.agent, detail, to_string(input),
                 timeout_ms: timeout_ms
               ) do
          {:ok, normalize_reply(reply)}
        end

      case result do
        {:ok, reply} ->
          send_ok(state.socket, id, reply)

        {:error, {:remote, code, message}} ->
          send_error(state.socket, id, code, message)

        {:error, {code, message}} ->
          send_error(state.socket, id, code, message)

        {:error, reason} ->
          send_error(state.socket, id, "call_failed", format_reason(reason))
      end

      state
    end)
  end

  defp handle_request(%{"id" => id, "op" => "tool.list"}, state) do
    with_scope(state, id, "tool.list", fn ->
      case Toolbox.list(state.binding.identity) do
        {:ok, tools} ->
          send_ok(state.socket, id, %{"tools" => Enum.map(tools, &normalize_tool/1)})
          state

        {:error, reason} ->
          send_error(state.socket, id, "tool_list_failed", format_reason(reason))
          state
      end
    end)
  end

  defp handle_request(%{"id" => id, "op" => "tool.invoke", "params" => params}, state) do
    with_scope(state, id, "tool.invoke", fn ->
      command = Map.get(params, "command")
      argv = normalize_argv(Map.get(params, "argv", []))
      timeout_ms = Map.get(params, "timeout_ms", @default_timeout_ms)
      app_session_id = blank_to_nil(Map.get(params, "app_session_id"))

      result =
        with true <-
               (is_binary(command) and String.trim(command) != "") or
                 {:error, {"invalid_command", "command is required"}},
             {:ok, tool} <- Toolbox.get(state.binding.identity, command),
             {:ok, built} <- Toolbox.build_invocation(tool, argv) do
          {:ok, tool, built}
        end

      case result do
        {:ok, tool, %{input: input, invocation: invocation}} ->
          case invocation["mode"] || "request_reply" do
            "stream" ->
              if allow_scope?(state, "stream") do
                open_tool_stream(state, id, tool, input, invocation, app_session_id, params)
              else
                send_error(state.socket, id, "forbidden", "token does not allow stream")
                state
              end

            _ ->
              invoke_tool_request_reply(state, id, tool, input, invocation, timeout_ms)
          end

        {:error, :not_found} ->
          send_error(state.socket, id, "tool_not_found", "installed tool not found")
          state

        {:error, {code, message}} ->
          send_error(state.socket, id, code, message)
          state

        {:error, reason} ->
          send_error(state.socket, id, "tool_invoke_failed", format_reason(reason))
          state
      end
    end)
  end

  defp handle_request(%{"id" => id, "op" => "stream.open", "params" => params}, state) do
    with_scope(state, id, "stream", fn ->
      peer = Map.get(params, "peer")
      capability_id = Map.get(params, "capability")
      input = to_string(Map.get(params, "input", ""))
      app_session_id = blank_to_nil(Map.get(params, "app_session_id"))

      result =
        with true <-
               (is_binary(peer) and peer != "") or {:error, {"invalid_peer", "peer is required"}},
             true <-
               (is_binary(capability_id) and capability_id != "") or
                 {:error, {"invalid_capability", "capability is required"}},
             true <-
               is_nil(app_session_id) or not Map.has_key?(state.streams, app_session_id) or
                 {:error, {"stream_exists", "stream session already active"}},
             {:ok, detail} <-
               CapabilityDiscovery.fetch_detail(state.binding.agent, peer, capability_id),
             {:ok, handle} <-
               CapabilityInvocation.open_stream(
                 state.binding.agent,
                 detail,
                 input,
                 maybe_app_session_opt(app_session_id)
               ) do
          {:ok, handle}
        end

      case result do
        {:ok, handle} ->
          send_ok(state.socket, id, %{
            "app_session_id" => handle.app_session_id,
            "request_id" => encode_hex(handle.request_id)
          })

          {pump_pid, pump_ref} = start_stream_pump(self(), state.binding.agent, handle)

          stream = %{
            handle: handle,
            pump_pid: pump_pid,
            pump_ref: pump_ref
          }

          %{state | streams: Map.put(state.streams, handle.app_session_id, stream)}

        {:error, {:remote, code, message}} ->
          send_error(state.socket, id, code, message)
          state

        {:error, {code, message}} ->
          send_error(state.socket, id, code, message)
          state

        {:error, reason} ->
          send_error(state.socket, id, "stream_open_failed", format_reason(reason))
          state
      end
    end)
  end

  defp handle_request(%{"id" => id, "op" => "stream.write", "params" => params}, state) do
    with_scope(state, id, "stream", fn ->
      app_session_id = Map.get(params, "app_session_id")
      data = Map.get(params, "data", "")

      with {:ok, stream} <- fetch_stream(state, app_session_id),
           :ok <-
             CapabilityInvocation.send_stream_data(
               state.binding.agent,
               stream.handle,
               to_string(data)
             ) do
        send_ok(state.socket, id, %{"app_session_id" => app_session_id, "written" => true})
        state
      else
        {:error, {code, message}} ->
          send_error(state.socket, id, code, message)
          state

        {:error, reason} ->
          send_error(state.socket, id, "stream_write_failed", format_reason(reason))
          state
      end
    end)
  end

  defp handle_request(%{"id" => id, "op" => "stream.resize", "params" => params}, state) do
    with_scope(state, id, "stream", fn ->
      app_session_id = Map.get(params, "app_session_id")

      with {:ok, stream} <- fetch_stream(state, app_session_id),
           {:ok, cols} <- positive_integer(Map.get(params, "cols"), "cols"),
           {:ok, rows} <- positive_integer(Map.get(params, "rows"), "rows"),
           :ok <-
             CapabilityInvocation.resize_stream(state.binding.agent, stream.handle, cols, rows) do
        send_ok(state.socket, id, %{
          "app_session_id" => app_session_id,
          "cols" => cols,
          "rows" => rows
        })

        state
      else
        {:error, {code, message}} ->
          send_error(state.socket, id, code, message)
          state

        {:error, reason} ->
          send_error(state.socket, id, "stream_resize_failed", format_reason(reason))
          state
      end
    end)
  end

  defp handle_request(%{"id" => id, "op" => "stream.close", "params" => params}, state) do
    with_scope(state, id, "stream", fn ->
      app_session_id = Map.get(params, "app_session_id")
      body = to_string(Map.get(params, "body", ""))

      with {:ok, stream} <- fetch_stream(state, app_session_id),
           :ok <- CapabilityInvocation.close_stream(state.binding.agent, stream.handle, body) do
        send_ok(state.socket, id, %{"app_session_id" => app_session_id, "closing" => true})
        state
      else
        {:error, {code, message}} ->
          send_error(state.socket, id, code, message)
          state

        {:error, reason} ->
          send_error(state.socket, id, "stream_close_failed", format_reason(reason))
          state
      end
    end)
  end

  defp handle_request(%{"id" => id, "op" => op}, state) do
    send_error(state.socket, id, "unsupported_operation", "unsupported operation #{op}")
    state
  end

  defp handle_request(_request, state) do
    send_error(state.socket, nil, "invalid_request", "request must include id and op")
    state
  end

  defp request_token(params) when is_map(params) do
    case Map.get(params, "token") do
      value when is_binary(value) -> String.trim(value)
      _ -> nil
    end
  end

  defp request_token(_params), do: nil

  defp authenticate_admin_token(state, token) do
    with {:ok, record} <- Service.authenticate_token(state.service, token),
         true <- record.kind == :admin or {:error, :forbidden} do
      {:ok, record}
    else
      {:error, reason} -> {:error, reason}
      false -> {:error, :forbidden}
    end
  end

  defp ensure_delegated_token(%{kind: :delegated, identity: %Identity{}}), do: :ok
  defp ensure_delegated_token(%{kind: :admin}), do: {:error, :admin_token_forbidden}
  defp ensure_delegated_token(_record), do: {:error, :unauthorized}

  defp ensure_identity_matches(%{identity: %Identity{} = _identity}, nil), do: :ok
  defp ensure_identity_matches(%{identity: %Identity{} = _identity}, ""), do: :ok

  defp ensure_identity_matches(%{identity: %Identity{} = identity}, query)
       when is_binary(query) do
    case AgentPool.resolve_identity(query) do
      {:ok, resolved} ->
        if resolved.public_key == identity.public_key do
          :ok
        else
          {:error, :identity_mismatch}
        end

      {:error, reason} ->
        {:error, reason}
    end
  end

  defp with_scope(state, id, scope, fun) do
    if allow_scope?(state, scope) do
      fun.()
    else
      send_error(state.socket, id, "forbidden", "token does not allow #{scope}")
      state
    end
  end

  defp allow_scope?(%{binding: %{token: token}}, scope), do: Token.allow_scope?(token, scope)
  defp allow_scope?(_state, _scope), do: false

  defp normalize_scopes_param(scopes) when is_list(scopes), do: scopes

  defp normalize_scopes_param(scopes) when is_binary(scopes) do
    scopes
    |> String.split(",", trim: true)
    |> Enum.map(&String.trim/1)
  end

  defp normalize_scopes_param(_scopes), do: Token.delegated_scopes()

  defp token_issue_error_code(:unauthorized), do: "unauthorized"
  defp token_issue_error_code(:expired), do: "token_expired"
  defp token_issue_error_code(:forbidden), do: "forbidden"
  defp token_issue_error_code(:identity_required), do: "identity_required"
  defp token_issue_error_code(:not_found), do: "identity_not_found"
  defp token_issue_error_code(:ambiguous), do: "identity_ambiguous"
  defp token_issue_error_code(:invalid_scope), do: "invalid_scope"
  defp token_issue_error_code(_reason), do: "token_issue_failed"

  defp token_issue_error_message(:unauthorized), do: "admin token is required"
  defp token_issue_error_message(:expired), do: "token has expired"
  defp token_issue_error_message(:forbidden), do: "token does not allow issuing delegated tokens"
  defp token_issue_error_message(:identity_required), do: "identity is required"
  defp token_issue_error_message(:not_found), do: "identity not found"
  defp token_issue_error_message(:ambiguous), do: "identity query is ambiguous"
  defp token_issue_error_message(:invalid_scope), do: "no valid scopes requested"
  defp token_issue_error_message(reason), do: format_reason(reason)

  defp initialize_error_code(:unauthorized), do: "unauthorized"
  defp initialize_error_code(:expired), do: "token_expired"
  defp initialize_error_code(:admin_token_forbidden), do: "forbidden"
  defp initialize_error_code(:identity_mismatch), do: "identity_mismatch"
  defp initialize_error_code(:not_found), do: "identity_not_found"
  defp initialize_error_code(:ambiguous), do: "identity_ambiguous"
  defp initialize_error_code(:no_default), do: "no_identity"
  defp initialize_error_code(_reason), do: "initialize_failed"

  defp initialize_error_message(:unauthorized), do: "delegated token is required"
  defp initialize_error_message(:expired), do: "token has expired"

  defp initialize_error_message(:admin_token_forbidden),
    do: "admin tokens cannot initialize identity sessions"

  defp initialize_error_message(:identity_mismatch), do: "token is bound to a different identity"
  defp initialize_error_message(:not_found), do: "identity not found"
  defp initialize_error_message(:ambiguous), do: "identity query is ambiguous"
  defp initialize_error_message(:no_default), do: "no active key available"
  defp initialize_error_message(reason), do: format_reason(reason)

  defp auth_error_code(:unauthorized), do: "unauthorized"
  defp auth_error_code(:expired), do: "token_expired"
  defp auth_error_code(:forbidden), do: "forbidden"
  defp auth_error_code(_reason), do: "auth_failed"

  defp auth_error_message(:unauthorized), do: "admin token is required"
  defp auth_error_message(:expired), do: "token has expired"
  defp auth_error_message(:forbidden), do: "token does not allow this operation"
  defp auth_error_message(reason), do: format_reason(reason)

  defp request_document(agent, to, path) do
    request_id = Frame.new_request_id()

    with {:ok, _entry} <- Agent.connect(agent, to),
         :ok <-
           Agent.send_message(agent, to, "",
             request_id: request_id,
             meta: %{"method" => "GET", "path" => path}
           ) do
      with {:ok, msg} <- wait_for_reply_message(agent, request_id, @default_timeout_ms),
           {:ok, document} <- decode_document(msg) do
        {:ok, document}
      end
    else
      {:error, reason} ->
        {:error, reason}

      other ->
        other
    end
  end

  defp send_message(agent, to, message) when is_binary(to) do
    with {:ok, _entry} <- Agent.connect(agent, to) do
      request_id = Frame.new_request_id()

      case Agent.send_message(agent, to, message,
             request_id: request_id,
             meta: %{"method" => "RAW", "path" => "/"}
           ) do
        :ok -> {:ok, request_id}
        {:error, reason} -> {:error, reason}
      end
    end
  end

  defp send_message(_agent, _to, _message), do: {:error, :invalid_peer}

  defp wait_for_reply_message(_agent, _request_id, timeout_ms) when timeout_ms <= 0 do
    {:error, :timeout}
  end

  defp wait_for_reply_message(agent, request_id, timeout_ms) do
    Process.sleep(100)
    Agent.poll_mailbox(agent)
    Process.sleep(10)

    matcher = fn message ->
      message[:request_id] == request_id and message[:kind] in [:response, :error]
    end

    case Agent.take_inbox(agent, matcher) do
      [] ->
        wait_for_reply_message(agent, request_id, timeout_ms - 110)

      [match | _] ->
        {:ok, match}
    end
  end

  defp decode_document(%{kind: :error, error_code: code, error_message: message}) do
    {:error, {:remote, code || "error", message || "unknown error"}}
  end

  defp decode_document(%{kind: :response, text: text}) do
    try do
      {:ok, :json.decode(text)}
    rescue
      _ -> {:error, :invalid_json}
    end
  end

  defp decode_document(_msg), do: {:error, :unexpected_reply}

  defp normalize_discovery(%{
         query: query,
         total: total,
         truncated?: truncated?,
         matches: matches
       }) do
    %{
      "query" => query,
      "total" => total,
      "truncated" => truncated?,
      "matches" =>
        Enum.map(matches, fn %{provider: provider, capability: capability} ->
          %{"provider" => provider, "capability" => capability}
        end)
    }
  end

  defp normalize_reply(reply) do
    %{
      "kind" => Atom.to_string(reply.kind),
      "from" => reply.from,
      "text" => reply.text,
      "request_id" => encode_hex(reply.request_id),
      "meta" => reply.meta || %{}
    }
  end

  defp normalize_tool(tool) do
    capability = tool["capability"] || %{}
    invocation = capability["invocation"] || %{}

    %{
      "command" => tool["command"],
      "summary" => Toolbox.command_summary(tool["command"] || "", capability),
      "usage" =>
        tool["usage"] || Toolbox.usage_from_capability(tool["command"] || "", capability),
      "provider" => tool["provider"],
      "capability_id" => tool["capability_id"] || capability["id"],
      "channel" => tool["channel"],
      "release_version" => tool["release_version"],
      "mode" => invocation["mode"] || "request_reply"
    }
  end

  defp identity_document(identity) do
    %{
      "name" => Identity.name(identity),
      "short_name" => Identity.short_name(identity),
      "public_key" => encode_hex(identity.public_key)
    }
  end

  defp send_ok(socket, id, result) do
    send_json(socket, %{"id" => id, "ok" => true, "result" => result})
  end

  defp send_error(socket, id, code, message) do
    send_json(socket, %{
      "id" => id,
      "ok" => false,
      "error" => %{"code" => to_string(code), "message" => to_string(message)}
    })
  end

  defp send_json(socket, payload) do
    encoded =
      payload
      |> :json.encode()
      |> IO.iodata_to_binary()

    :socket.send(socket, [encoded, "\n"])
  end

  defp decode_request(line) do
    try do
      {:ok, :json.decode(line)}
    rescue
      error -> {:error, error}
    end
  end

  defp read_loop(socket, owner, buffer) do
    case :socket.recv(socket, 0) do
      {:ok, data} when is_binary(data) ->
        {lines, rest} = split_lines(buffer <> data)
        Enum.each(lines, &send(owner, {:socket_line, &1}))
        read_loop(socket, owner, rest)

      {:error, reason} ->
        send(owner, {:socket_closed, reason})
        :ok
    end
  end

  defp split_lines(buffer) do
    case String.split(buffer, "\n") do
      [single] ->
        {[], single}

      parts ->
        {Enum.drop(parts, -1), List.last(parts)}
    end
  end

  defp encode_hex(nil), do: nil
  defp encode_hex(binary), do: Base.encode16(binary, case: :lower)

  defp fetch_stream(_state, app_session_id)
       when not is_binary(app_session_id) or app_session_id == "" do
    {:error, {"invalid_stream", "app_session_id is required"}}
  end

  defp fetch_stream(state, app_session_id) do
    case Map.get(state.streams, app_session_id) do
      nil -> {:error, {"stream_not_found", "stream session not found"}}
      stream -> {:ok, stream}
    end
  end

  defp normalize_argv(argv) when is_list(argv), do: Enum.map(argv, &to_string/1)
  defp normalize_argv(_argv), do: []

  defp positive_integer(value, _field) when is_integer(value) and value > 0, do: {:ok, value}
  defp positive_integer(_value, field), do: {:error, {"invalid_#{field}", "#{field} must be > 0"}}

  defp blank_to_nil(value) when is_binary(value) do
    case String.trim(value) do
      "" -> nil
      trimmed -> trimmed
    end
  end

  defp blank_to_nil(_value), do: nil

  defp maybe_app_session_opt(nil), do: []
  defp maybe_app_session_opt(app_session_id), do: [app_session_id: app_session_id]

  defp start_stream_pump(owner, agent, handle) do
    spawn_monitor(fn -> pump_stream(owner, agent, handle) end)
  end

  defp pump_stream(owner, agent, handle) do
    case CapabilityInvocation.recv_stream(agent, handle, timeout_ms: 5_000) do
      {:ok, message} ->
        send(owner, {:host_stream_event, handle.app_session_id, normalize_stream_event(message)})

        if terminal_message?(message) do
          :ok
        else
          pump_stream(owner, agent, handle)
        end

      {:error, :timeout} ->
        pump_stream(owner, agent, handle)

      {:error, reason} ->
        send(
          owner,
          {:host_stream_event, handle.app_session_id, stream_error_event(handle, reason)}
        )

        :ok
    end
  end

  defp normalize_stream_event(message) do
    base = %{
      "op" => "event",
      "event" => Atom.to_string(message.kind),
      "from" => message.from,
      "text" => message.text,
      "request_id" => encode_hex(message.request_id),
      "app_session_id" => message.app_session_id,
      "meta" => message.meta || %{}
    }

    base
    |> maybe_put("status", get_in(message, [:meta, "status"]))
    |> maybe_put("code", Map.get(message, :error_code))
    |> maybe_put("message", Map.get(message, :error_message))
  end

  defp stream_error_event(handle, reason) do
    %{
      "op" => "event",
      "event" => "stream_error",
      "from" => nil,
      "text" => "",
      "request_id" => encode_hex(handle.request_id),
      "app_session_id" => handle.app_session_id,
      "meta" => %{},
      "code" => "stream_recv_failed",
      "message" => format_reason(reason)
    }
  end

  defp terminal_message?(%{kind: kind})
       when kind in [:stream_exit, :stream_error, :error, :response],
       do: true

  defp terminal_message?(_message), do: false

  defp final_event_stream_id(app_session_id, %{"event" => event})
       when event in ["stream_exit", "stream_error", "error", "response"],
       do: app_session_id

  defp final_event_stream_id(_app_session_id, _event), do: nil

  defp maybe_drop_stream(state, nil), do: state

  defp maybe_drop_stream(state, app_session_id) do
    case Map.pop(state.streams, app_session_id) do
      {nil, _streams} ->
        state

      {stream, streams} ->
        if is_pid(stream.pump_pid) and Process.alive?(stream.pump_pid) do
          Process.exit(stream.pump_pid, :normal)
        end

        %{state | streams: streams}
    end
  end

  defp close_streams(%{binding: %{agent: agent}, streams: streams}) when is_pid(agent) do
    Enum.each(streams, fn {_app_session_id, stream} ->
      _ =
        try do
          CapabilityInvocation.close_stream(agent, stream.handle)
        catch
          :exit, _reason -> :ok
        end

      if is_pid(stream.pump_pid) and Process.alive?(stream.pump_pid) do
        Process.exit(stream.pump_pid, :normal)
      end
    end)
  end

  defp close_streams(_state), do: :ok

  defp invoke_tool_request_reply(state, id, tool, input, invocation, timeout_ms) do
    case CapabilityInvocation.invoke(state.binding.agent, tool, input,
           invocation_override: invocation,
           timeout_ms: timeout_ms
         ) do
      {:ok, reply} ->
        send_ok(state.socket, id, normalize_reply(reply))
        state

      {:error, {:remote, code, message}} ->
        send_error(state.socket, id, code, message)
        state

      {:error, reason} ->
        send_error(state.socket, id, "tool_invoke_failed", format_reason(reason))
        state
    end
  end

  defp open_tool_stream(state, id, tool, input, invocation, app_session_id, params) do
    if not is_nil(app_session_id) and Map.has_key?(state.streams, app_session_id) do
      send_error(state.socket, id, "stream_exists", "stream session already active")
      state
    else
      case CapabilityInvocation.open_stream(
             state.binding.agent,
             tool,
             input,
             maybe_app_session_opt(app_session_id) ++ [invocation_override: invocation]
           ) do
        {:ok, handle} ->
          maybe_send_initial_resize(state.binding.agent, handle, params)

          send_ok(state.socket, id, %{
            "mode" => "stream",
            "app_session_id" => handle.app_session_id,
            "request_id" => encode_hex(handle.request_id)
          })

          {pump_pid, pump_ref} = start_stream_pump(self(), state.binding.agent, handle)

          stream = %{
            handle: handle,
            pump_pid: pump_pid,
            pump_ref: pump_ref
          }

          %{state | streams: Map.put(state.streams, handle.app_session_id, stream)}

        {:error, {:remote, code, message}} ->
          send_error(state.socket, id, code, message)
          state

        {:error, reason} ->
          send_error(state.socket, id, "tool_invoke_failed", format_reason(reason))
          state
      end
    end
  end

  defp maybe_send_initial_resize(agent, handle, %{"cols" => cols, "rows" => rows})
       when is_integer(cols) and cols > 0 and is_integer(rows) and rows > 0 do
    _ = CapabilityInvocation.resize_stream(agent, handle, cols, rows)
    :ok
  end

  defp maybe_send_initial_resize(_agent, _handle, _params), do: :ok

  defp maybe_put(map, _key, nil), do: map
  defp maybe_put(map, key, value), do: Map.put(map, key, value)

  defp format_reason({code, message}), do: "#{code}: #{message}"
  defp format_reason(reason), do: inspect(reason)
end
