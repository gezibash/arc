defmodule Arc.Data.CapabilityInvocation do
  @moduledoc """
  Invoke an installed ARC capability package over the framed ARC request path.

  Supports both one-shot request/reply invocations and long-lived stream
  sessions keyed by `app_session_id`.
  """

  alias Arc.Data.Agent
  alias Arc.Data.Frame

  @default_timeout_ms 10_000

  @type stream_handle :: %{
          request_id: binary(),
          app_session_id: String.t(),
          peer_query: String.t(),
          provider: map(),
          capability: map(),
          invocation: map()
        }

  @spec invoke(pid(), map(), binary(), keyword()) :: {:ok, map()} | {:error, term()}
  def invoke(agent, mount, input, opts \\ [])

  def invoke(agent, %{"provider" => provider, "capability" => capability}, input, opts)
      when is_binary(input) and is_map(provider) and is_map(capability) do
    timeout_ms = Keyword.get(opts, :timeout_ms, @default_timeout_ms)
    request_id = Frame.new_request_id()

    with {:ok, peer_query} <- peer_query(provider),
         invocation <- invocation_for(capability, opts),
         :request_reply <- invocation_mode(invocation) |> mode_atom(),
         {:ok, _entry} <- Agent.connect(agent, peer_query),
         :ok <-
           Agent.send_message(agent, peer_query, input,
             request_id: request_id,
             meta: invocation_meta(invocation, request_id, opts)
           ),
         {:ok, reply} <- wait_for_reply(agent, request_id, timeout_ms) do
      decode_reply(reply)
    else
      :stream -> {:error, :stream_invocation}
      error -> error
    end
  end

  def invoke(_agent, _mount, _input, _opts), do: {:error, :invalid_mount}

  @spec open_stream(pid(), map(), binary(), keyword()) ::
          {:ok, stream_handle()} | {:error, term()}
  def open_stream(agent, mount, input, opts \\ [])

  def open_stream(agent, %{"provider" => provider, "capability" => capability}, input, opts)
      when is_binary(input) and is_map(provider) and is_map(capability) do
    request_id = Frame.new_request_id()
    app_session_id = Keyword.get(opts, :app_session_id, new_app_session_id())

    with {:ok, peer_query} <- peer_query(provider),
         invocation <- invocation_for(capability, opts),
         :stream <- invocation_mode(invocation) |> mode_atom(),
         {:ok, _entry} <- Agent.connect(agent, peer_query),
         meta <-
           invocation_meta(
             invocation,
             request_id,
             Keyword.put(opts, :app_session_id, app_session_id)
           ),
         payload <- Frame.encode_stream_open(request_id, meta, input),
         :ok <- Agent.send_message(agent, peer_query, payload, raw: true) do
      {:ok,
       %{
         request_id: request_id,
         app_session_id: app_session_id,
         peer_query: peer_query,
         provider: provider,
         capability: capability,
         invocation: invocation
       }}
    else
      :request_reply -> {:error, :request_reply_invocation}
      error -> error
    end
  end

  def open_stream(_agent, _mount, _input, _opts), do: {:error, :invalid_mount}

  @spec send_stream_data(pid(), stream_handle(), binary()) :: :ok | {:error, term()}
  def send_stream_data(agent, stream, data) when is_binary(data) do
    send_stream_frame(agent, stream, :stream_data, data, %{})
  end

  @spec resize_stream(pid(), stream_handle(), pos_integer(), pos_integer()) ::
          :ok | {:error, term()}
  def resize_stream(agent, stream, cols, rows)
      when is_integer(cols) and cols > 0 and is_integer(rows) and rows > 0 do
    send_stream_frame(agent, stream, :stream_resize, "", %{"cols" => cols, "rows" => rows})
  end

  def resize_stream(_agent, _stream, _cols, _rows), do: {:error, :invalid_resize}

  @spec close_stream(pid(), stream_handle(), binary()) :: :ok | {:error, term()}
  def close_stream(agent, stream, body \\ "") when is_binary(body) do
    send_stream_frame(agent, stream, :stream_close, body, %{})
  end

  @spec recv_stream(pid(), stream_handle(), keyword()) :: {:ok, map()} | {:error, :timeout}
  def recv_stream(agent, stream, opts \\ []) do
    timeout_ms = Keyword.get(opts, :timeout_ms, @default_timeout_ms)
    started_at = System.monotonic_time(:millisecond)
    do_recv_stream(agent, stream, timeout_ms, started_at)
  end

  defp do_recv_stream(agent, stream, timeout_ms, started_at) do
    Process.sleep(50)
    Agent.poll_mailbox(agent)
    Process.sleep(10)

    matcher = fn message ->
      message[:request_id] == stream.request_id and
        (message[:app_session_id] == stream.app_session_id or
           message[:kind] in [:error, :response]) and
        message[:kind] in [:stream_data, :stream_exit, :stream_error, :error, :response]
    end

    case Agent.take_inbox(agent, matcher) do
      [] ->
        if elapsed_ms(started_at) > timeout_ms do
          {:error, :timeout}
        else
          do_recv_stream(agent, stream, timeout_ms, started_at)
        end

      [match | _] ->
        {:ok, match}
    end
  end

  defp send_stream_frame(agent, %{peer_query: peer_query} = stream, frame_type, body, extra_meta)
       when frame_type in [:stream_data, :stream_resize, :stream_close] and is_map(extra_meta) do
    meta = stream_meta(stream, extra_meta)

    payload =
      case frame_type do
        :stream_data -> Frame.encode_stream_data(stream.request_id, meta, body)
        :stream_resize -> Frame.encode_stream_resize(stream.request_id, meta, body)
        :stream_close -> Frame.encode_stream_close(stream.request_id, meta, body)
      end

    Agent.send_message(agent, peer_query, payload, raw: true)
  end

  defp wait_for_reply(agent, request_id, timeout_ms) do
    started_at = System.monotonic_time(:millisecond)
    wait_for_reply(agent, request_id, timeout_ms, started_at)
  end

  defp wait_for_reply(agent, request_id, timeout_ms, started_at) do
    Process.sleep(100)
    Agent.poll_mailbox(agent)
    Process.sleep(10)

    matcher = fn message ->
      message[:request_id] == request_id and message[:kind] in [:response, :error]
    end

    case Agent.take_inbox(agent, matcher) do
      [] ->
        if elapsed_ms(started_at) > timeout_ms do
          {:error, :timeout}
        else
          wait_for_reply(agent, request_id, timeout_ms, started_at)
        end

      [match | _] ->
        {:ok, match}
    end
  end

  defp invocation_meta(invocation, _request_id, opts) do
    meta = %{
      "method" => invocation["method"] || "RAW",
      "path" => invocation["path"] || "/"
    }

    case Keyword.get(opts, :app_session_id) do
      sid when is_binary(sid) and sid != "" -> Map.put(meta, "app_session_id", sid)
      _ -> meta
    end
  end

  defp stream_meta(stream, extra_meta) do
    stream.invocation
    |> invocation_meta(stream.request_id, app_session_id: stream.app_session_id)
    |> Map.merge(extra_meta)
  end

  defp invocation_for(capability, opts) do
    override = Keyword.get(opts, :invocation_override)
    merge_invocation(capability["invocation"] || %{}, override)
  end

  defp merge_invocation(base, override) when is_map(override), do: Map.merge(base, override)
  defp merge_invocation(base, _override), do: base

  defp invocation_mode(invocation) do
    invocation["mode"] || "request_reply"
  end

  defp mode_atom("stream"), do: :stream
  defp mode_atom(_mode), do: :request_reply

  defp peer_query(provider) do
    case provider["name"] || provider["short_name"] do
      value when is_binary(value) and value != "" -> {:ok, value}
      _ -> {:error, :invalid_mount}
    end
  end

  defp new_app_session_id do
    "app-" <> Base.encode16(:crypto.strong_rand_bytes(8), case: :lower)
  end

  defp elapsed_ms(started_at) do
    System.monotonic_time(:millisecond) - started_at
  end

  defp decode_reply(%{kind: :response} = reply), do: {:ok, reply}

  defp decode_reply(%{kind: :error, error_code: code, error_message: message}) do
    {:error, {:remote, code || "error", message || "unknown error"}}
  end

  defp decode_reply(_reply), do: {:error, :unexpected_reply}
end
