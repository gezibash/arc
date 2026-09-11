defmodule Arc.Data.Handler.Exec do
  @moduledoc """
  Generic provider runtime adapter.

  Providers own their runtime executable and capability manifest. ARC owns
  identity, framing, transport, manifest validation, install/invocation, and
  stream/session routing.

  The executable communicates via newline-delimited JSON on stdin/stdout.
  ARC reads stdout in chunks and joins the chunks until it sees the newline,
  so a provider can return a large one-shot reply (for example a base64
  attachment) on a single line. A line is limited to 64 MiB. If a line exceeds
  the limit, ARC drops the line, logs a warning, and fails the oldest pending
  request with a `line_too_long` error.

  Incoming events from ARC:

    request       -> one-shot request/reply invocation
    stream_open   -> open a long-lived application stream
    stream_data   -> write stream input
    stream_resize -> terminal/window resize metadata
    stream_close  -> close a stream

  Provider responses:

    {"reply":"..."}                                         legacy one-shot reply
    {"error":"..."}                                         legacy one-shot error
    {"op":"reply","request_id":"...","reply":"..."}         typed one-shot reply
    {"op":"error","request_id":"...","error":"..."}         typed one-shot error
    {"op":"stream_data","app_session_id":"...","data":"..."}
    {"op":"stream_exit","app_session_id":"...","status":0}
    {"op":"stream_error","app_session_id":"...","message":"..."}

  URI:
    exec:///path/to/runtime?manifest=/abs/path/to/package.(json|toml)

  Runtime environment injected by `arc serve`:
    ARC_HOST_SOCKET
    ARC_HOST_TOKEN
    ARC_IDENTITY
    ARC_IDENTITY_SHORT
    ARC_PUBLIC_KEY
  """

  @behaviour Arc.Data.Handler

  require Logger

  # Hard ceiling on one provider stdout line. Protects the VM from a provider
  # that never sends a newline.
  @max_line_bytes 64 * 1024 * 1024

  # Chunk size for reading provider stdout. Lines longer than this arrive as
  # several `:noeol` chunks and are joined in `handle_info/2`. Not a cap.
  @line_chunk_bytes 65_536

  @impl true
  def init(uri) when is_binary(uri) do
    parsed = URI.parse(uri)
    executable = parsed.path |> decode_path() |> Path.expand()
    params = decode_query(parsed.query)

    with true <- File.exists?(executable) or {:error, {:not_found, executable}},
         {:ok, package} <- load_package(executable, params),
         {:ok, port} <- open_port(executable, parse_args(params), runtime_env(params)) do
      {:ok,
       %{
         port: port,
         executable: executable,
         manifest_path: manifest_path(executable, params),
         package: package,
         pending_requests: %{},
         pending_order: [],
         stream_sessions: %{},
         line_buffer: [],
         line_buffer_bytes: 0
       }}
    else
      false -> {:error, {:not_found, executable}}
      {:error, _reason} = error -> error
    end
  end

  @impl true
  def handle_message(message, from_pk, state) do
    handle_message(message, from_pk, %{}, state)
  end

  @impl true
  def handle_message(message, from_pk, context, state) when is_map(context) do
    correlation_id = request_correlation_id(context)

    payload =
      base_event_payload("request", message, from_pk, context)
      |> Map.put("request_id", correlation_id)

    state =
      state
      |> put_pending_request(correlation_id, from_pk, context)
      |> send_port_payload(payload)

    {:noreply, state}
  end

  @impl true
  def handle_frame(type, message, from_pk, context, state)
      when type in [:stream_open, :stream_data, :stream_resize, :stream_close] and is_map(context) do
    app_session_id = require_app_session_id(context)

    payload =
      base_event_payload(to_string(type), message, from_pk, context)
      |> Map.put("app_session_id", app_session_id)
      |> maybe_put_resize_meta(type, context)

    state =
      case type do
        :stream_open ->
          put_stream_session(state, app_session_id, from_pk, context)

        _ ->
          state
      end
      |> send_port_payload(payload)

    {:noreply, state}
  end

  @impl true
  def handle_info({port, {:data, {:noeol, chunk}}}, %{port: port} = state) do
    case buffer_chunk(state, chunk) do
      {:ok, state} -> {:noreply, state}
      {:overflow, state} -> fail_oversized_line(state)
    end
  end

  def handle_info({port, {:data, {:eol, chunk}}}, %{port: port} = state) do
    case buffer_chunk(state, chunk) do
      {:ok, %{line_buffer: :discard} = state} ->
        {:noreply, reset_line_buffer(state)}

      {:ok, state} ->
        line = IO.iodata_to_binary(state.line_buffer)
        state = reset_line_buffer(state)

        case dispatch_provider_event(decode_provider_event(line), state) do
          :unhandled -> {:noreply, state}
          result -> result
        end

      {:overflow, state} ->
        fail_oversized_line(reset_line_buffer(state))
    end
  end

  # A line that already overflowed is marked :discard; later chunks of that
  # line are dropped until the newline arrives.
  defp buffer_chunk(%{line_buffer: :discard} = state, _chunk), do: {:ok, state}

  defp buffer_chunk(state, chunk) do
    bytes = state.line_buffer_bytes + byte_size(chunk)

    if bytes > @max_line_bytes do
      {:overflow, %{state | line_buffer: :discard, line_buffer_bytes: bytes}}
    else
      {:ok, %{state | line_buffer: [state.line_buffer, chunk], line_buffer_bytes: bytes}}
    end
  end

  defp reset_line_buffer(state), do: %{state | line_buffer: [], line_buffer_bytes: 0}

  defp fail_oversized_line(state) do
    Logger.warning(
      "provider #{state.executable} sent a stdout line over #{@max_line_bytes} bytes; dropped"
    )

    message = "provider reply line exceeds #{@max_line_bytes} bytes"

    case emit_request_error(state, nil, message) do
      :unhandled -> {:noreply, state}
      result -> result
    end
  end

  defp dispatch_provider_event(event, state) do
    case event do
      {:reply, correlation_id, reply} ->
        emit_request_event(
          state,
          correlation_id,
          :response,
          %{"status" => 200},
          normalize_text(reply)
        )

      {:error, correlation_id, error} ->
        emit_request_error(state, correlation_id, normalize_text(error))

      {:stream_data, app_session_id, meta, body} ->
        emit_stream_event(state, app_session_id, :stream_data, meta, body, false)

      {:stream_exit, app_session_id, meta, body} ->
        emit_stream_event(state, app_session_id, :stream_exit, meta, body, true)

      {:stream_error, app_session_id, meta, body} ->
        emit_stream_event(state, app_session_id, :stream_error, meta, body, true)

      :ignore ->
        :unhandled
    end
  end

  def handle_info({port, {:exit_status, code}}, %{port: port} = state) do
    events =
      Enum.flat_map(state.stream_sessions, fn {app_session_id, session} ->
        [
          %{
            to_pk: session.to_pk,
            frame_type: :stream_error,
            request_id: session.request_id,
            meta: %{
              "app_session_id" => app_session_id,
              "code" => "provider_exit",
              "message" => "provider exited with status #{code}"
            },
            body: ""
          }
        ]
      end)

    {:emit, events, %{state | stream_sessions: %{}, pending_requests: %{}, pending_order: []}}
  end

  def handle_info(_message, _state), do: :unhandled

  @impl true
  def capability(state) do
    get_in(state, [:package, "capability"]) || %{}
  end

  @impl true
  def package(state) do
    state.package || %{}
  end

  defp emit_request_event(state, correlation_id, frame_type, meta, body) do
    case pop_pending_request(state, correlation_id) do
      {nil, _state} ->
        :unhandled

      {pending, state} ->
        event =
          if pending.framed? do
            %{
              to_pk: pending.to_pk,
              frame_type: frame_type,
              request_id: pending.request_id,
              meta: meta,
              body: body
            }
          else
            %{to_pk: pending.to_pk, payload: body}
          end

        {:emit, [event], state}
    end
  end

  defp emit_request_error(state, correlation_id, message) do
    case pop_pending_request(state, correlation_id) do
      {nil, _state} ->
        :unhandled

      {pending, state} ->
        event =
          if pending.framed? do
            %{
              to_pk: pending.to_pk,
              frame_type: :error,
              request_id: pending.request_id,
              meta: %{"code" => "handler_error", "message" => message},
              body: ""
            }
          else
            %{to_pk: pending.to_pk, payload: "error: #{message}"}
          end

        {:emit, [event], state}
    end
  end

  defp emit_stream_event(state, app_session_id, frame_type, meta, body, remove?) do
    case Map.get(state.stream_sessions, app_session_id) do
      nil ->
        :unhandled

      session ->
        meta = Map.put(meta, "app_session_id", app_session_id)

        state =
          if remove? do
            %{state | stream_sessions: Map.delete(state.stream_sessions, app_session_id)}
          else
            state
          end

        {:emit,
         [
           %{
             to_pk: session.to_pk,
             frame_type: frame_type,
             request_id: session.request_id,
             meta: meta,
             body: body
           }
         ], state}
    end
  end

  defp base_event_payload(op, message, from_pk, context) do
    %{
      "op" => op,
      "message" => message,
      "from" => Base.encode16(from_pk, case: :lower),
      "meta" => Map.get(context, :meta, %{}),
      "arc_session_id" => encode_hex(Map.get(context, :arc_session_id)),
      "app_session_id" => Map.get(context, :app_session_id),
      "request_id" => encode_hex(Map.get(context, :request_id)),
      "framed" => Map.get(context, :framed?, false)
    }
  end

  defp maybe_put_resize_meta(payload, :stream_resize, context) do
    payload
    |> Map.put("cols", get_in(context, [:meta, "cols"]))
    |> Map.put("rows", get_in(context, [:meta, "rows"]))
  end

  defp maybe_put_resize_meta(payload, _type, _context), do: payload

  defp request_correlation_id(context) do
    case encode_hex(Map.get(context, :request_id)) do
      nil -> random_correlation_id()
      request_id -> request_id
    end
  end

  defp random_correlation_id do
    12
    |> :crypto.strong_rand_bytes()
    |> Base.encode16(case: :lower)
  end

  defp require_app_session_id(context) do
    case Map.get(context, :app_session_id) do
      sid when is_binary(sid) and sid != "" -> sid
      _ -> raise ArgumentError, "stream events require app_session_id"
    end
  end

  defp put_pending_request(state, correlation_id, to_pk, context) do
    request_id =
      case Map.get(context, :request_id) do
        request_id when is_binary(request_id) and byte_size(request_id) == 16 ->
          request_id

        _ ->
          Arc.Data.Frame.new_request_id()
      end

    pending = %{
      to_pk: to_pk,
      request_id: request_id,
      framed?: Map.get(context, :framed?, false)
    }

    %{
      state
      | pending_requests: Map.put(state.pending_requests, correlation_id, pending),
        pending_order: state.pending_order ++ [correlation_id]
    }
  end

  defp put_stream_session(state, app_session_id, to_pk, context) do
    request_id =
      case Map.get(context, :request_id) do
        request_id when is_binary(request_id) and byte_size(request_id) == 16 ->
          request_id

        _ ->
          Arc.Data.Frame.new_request_id()
      end

    session = %{
      to_pk: to_pk,
      request_id: request_id
    }

    %{state | stream_sessions: Map.put(state.stream_sessions, app_session_id, session)}
  end

  defp pop_pending_request(state, nil) do
    case state.pending_order do
      [correlation_id | rest] ->
        pending = Map.get(state.pending_requests, correlation_id)

        {pending,
         %{
           state
           | pending_requests: Map.delete(state.pending_requests, correlation_id),
             pending_order: rest
         }}

      [] ->
        {nil, state}
    end
  end

  defp pop_pending_request(state, correlation_id) do
    case Map.pop(state.pending_requests, correlation_id) do
      {nil, _pending_requests} ->
        {nil, state}

      {pending, pending_requests} ->
        pending_order = Enum.reject(state.pending_order, &(&1 == correlation_id))
        {pending, %{state | pending_requests: pending_requests, pending_order: pending_order}}
    end
  end

  defp send_port_payload(state, payload) do
    Port.command(state.port, [IO.iodata_to_binary(:json.encode(payload)), "\n"])
    state
  end

  defp decode_provider_event(line) do
    case :json.decode(line) do
      %{"reply" => reply, "request_id" => correlation_id} ->
        {:reply, maybe_string(correlation_id), reply}

      %{"error" => error, "request_id" => correlation_id} ->
        {:error, maybe_string(correlation_id), error}

      %{"reply" => reply} ->
        {:reply, nil, reply}

      %{"error" => error} ->
        {:error, nil, error}

      %{"op" => "reply", "request_id" => correlation_id, "reply" => reply} ->
        {:reply, maybe_string(correlation_id), reply}

      %{"op" => "error", "request_id" => correlation_id, "error" => error} ->
        {:error, maybe_string(correlation_id), error}

      %{"op" => "stream_data", "app_session_id" => app_session_id} = event ->
        {:stream_data, app_session_id, stream_meta(event),
         normalize_text(event["data"] || event["body"] || "")}

      %{"op" => "stream_exit", "app_session_id" => app_session_id} = event ->
        meta = stream_meta(event) |> maybe_put("status", event["status"])

        {:stream_exit, app_session_id, meta,
         normalize_text(event["message"] || event["body"] || "")}

      %{"op" => "stream_error", "app_session_id" => app_session_id} = event ->
        meta =
          stream_meta(event)
          |> maybe_put("code", maybe_string(event["code"]) || "stream_error")
          |> maybe_put("message", maybe_string(event["message"]) || "stream error")

        {:stream_error, app_session_id, meta, normalize_text(event["body"] || "")}

      _ ->
        :ignore
    end
  rescue
    _ -> :ignore
  end

  defp stream_meta(event) do
    %{}
    |> maybe_put("channel", maybe_string(event["channel"]))
    |> maybe_put("encoding", maybe_string(event["encoding"]))
  end

  defp load_package(executable, params) do
    case manifest_path(executable, params) do
      nil -> {:error, :missing_manifest}
      path -> Arc.Data.CapabilityPackage.load_file(path)
    end
  end

  defp manifest_path(executable, %{"manifest" => path}) when is_binary(path) and path != "" do
    if Path.type(path) == :absolute do
      Path.expand(path)
    else
      Path.expand(path, Path.dirname(executable))
    end
  end

  defp manifest_path(_executable, _params), do: nil

  defp open_port(executable, args, env) do
    port_opts =
      [
        :binary,
        :exit_status,
        {:line, @line_chunk_bytes}
      ] ++
        if(args == [], do: [], else: [{:args, args}]) ++
        if(env == [], do: [], else: [{:env, env}])

    {:ok, Port.open({:spawn_executable, executable}, port_opts)}
  rescue
    error -> {:error, error}
  end

  defp normalize_text(reply) when is_binary(reply), do: reply
  defp normalize_text(reply), do: reply |> :json.encode() |> IO.iodata_to_binary()

  defp maybe_string(value) when is_binary(value) and value != "", do: value
  defp maybe_string(_value), do: nil

  defp maybe_put(map, _key, nil), do: map
  defp maybe_put(map, key, value), do: Map.put(map, key, value)

  defp encode_hex(nil), do: nil
  defp encode_hex(binary), do: Base.encode16(binary, case: :lower)

  defp decode_query(nil), do: %{}
  defp decode_query(""), do: %{}
  defp decode_query(query), do: URI.decode_query(query)

  defp parse_args(%{"args" => encoded}) when is_binary(encoded) and encoded != "" do
    case :json.decode(encoded) do
      args when is_list(args) -> Enum.map(args, &to_string/1)
      _ -> []
    end
  rescue
    _ -> []
  end

  defp parse_args(_params), do: []

  defp runtime_env(params) when is_map(params) do
    []
    |> maybe_put_env("ARC_HOST_SOCKET", params["arc_host_socket"])
    |> maybe_put_env("ARC_HOST_TOKEN", params["arc_host_token"])
    |> maybe_put_env("ARC_IDENTITY", params["arc_identity"])
    |> maybe_put_env("ARC_IDENTITY_SHORT", params["arc_identity_short"])
    |> maybe_put_env("ARC_PUBLIC_KEY", params["arc_public_key"])
  end

  defp maybe_put_env(env, _key, nil), do: env
  defp maybe_put_env(env, _key, ""), do: env

  defp maybe_put_env(env, key, value),
    do: [{String.to_charlist(key), String.to_charlist(value)} | env]

  defp decode_path(nil), do: ""
  defp decode_path(path), do: URI.decode(path)
end
