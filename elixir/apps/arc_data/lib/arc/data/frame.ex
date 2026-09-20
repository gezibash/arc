defmodule Arc.Data.Frame do
  @moduledoc """
  Binary request/response frame for ARC handler RPC.

  Wire format:

      [1 byte]   version
      [1 byte]   type
                 1=request
                 2=response
                 3=error
                 4=stream_open
                 5=stream_data
                 6=stream_resize
                 7=stream_close
                 8=stream_exit
                 9=stream_error
                 10=event (server-initiated, meta.topic names it)
      [2 bytes]  flags
      [16 bytes] request_id
      [4 bytes]  meta_length (big-endian)
      [N bytes]  meta JSON
      [4 bytes]  body_length (big-endian)
      [N bytes]  body bytes
  """

  @version 1
  @request_type 1
  @response_type 2
  @error_type 3
  @stream_open_type 4
  @stream_data_type 5
  @stream_resize_type 6
  @stream_close_type 7
  @stream_exit_type 8
  @stream_error_type 9
  @event_type 10
  @request_id_bytes 16

  @type frame_type ::
          :request
          | :response
          | :error
          | :stream_open
          | :stream_data
          | :stream_resize
          | :stream_close
          | :stream_exit
          | :stream_error
          | :event

  @type decoded_frame :: %{
          version: pos_integer(),
          type: frame_type(),
          flags: non_neg_integer(),
          request_id: binary(),
          meta: map(),
          body: binary()
        }

  @doc """
  Create a new random request id.
  """
  @spec new_request_id() :: binary()
  def new_request_id do
    :crypto.strong_rand_bytes(@request_id_bytes)
  end

  @doc """
  Encode a request frame.
  """
  @spec encode_request(binary(), map(), binary()) :: binary()
  def encode_request(request_id, meta, body) when is_map(meta) and is_binary(body) do
    encode_frame(:request, request_id, meta, body)
  end

  @doc """
  Encode a response frame.
  """
  @spec encode_response(binary(), map(), binary()) :: binary()
  def encode_response(request_id, meta, body) when is_map(meta) and is_binary(body) do
    encode_frame(:response, request_id, meta, body)
  end

  @doc """
  Encode an error frame.
  """
  @spec encode_error(binary(), String.t(), String.t(), map(), binary()) :: binary()
  def encode_error(request_id, code, message, meta \\ %{}, body \\ "")
      when is_binary(code) and is_binary(message) and is_map(meta) and is_binary(body) do
    error_meta = Map.merge(%{"code" => code, "message" => message}, meta)
    encode_frame(:error, request_id, error_meta, body)
  end

  @spec encode_stream_open(binary(), map(), binary()) :: binary()
  def encode_stream_open(request_id, meta, body) when is_map(meta) and is_binary(body) do
    encode_frame(:stream_open, request_id, meta, body)
  end

  @spec encode_stream_data(binary(), map(), binary()) :: binary()
  def encode_stream_data(request_id, meta, body) when is_map(meta) and is_binary(body) do
    encode_frame(:stream_data, request_id, meta, body)
  end

  @spec encode_stream_resize(binary(), map(), binary()) :: binary()
  def encode_stream_resize(request_id, meta, body \\ "") when is_map(meta) and is_binary(body) do
    encode_frame(:stream_resize, request_id, meta, body)
  end

  @spec encode_stream_close(binary(), map(), binary()) :: binary()
  def encode_stream_close(request_id, meta, body \\ "") when is_map(meta) and is_binary(body) do
    encode_frame(:stream_close, request_id, meta, body)
  end

  @spec encode_stream_exit(binary(), map(), binary()) :: binary()
  def encode_stream_exit(request_id, meta, body \\ "") when is_map(meta) and is_binary(body) do
    encode_frame(:stream_exit, request_id, meta, body)
  end

  @spec encode_stream_error(binary(), String.t(), String.t(), map(), binary()) :: binary()
  def encode_stream_error(request_id, code, message, meta \\ %{}, body \\ "")
      when is_binary(code) and is_binary(message) and is_map(meta) and is_binary(body) do
    error_meta = Map.merge(%{"code" => code, "message" => message}, meta)
    encode_frame(:stream_error, request_id, error_meta, body)
  end

  @doc """
  Encode an event frame: a message a provider sends without a request.
  `topic` names the event. The request id is fresh; nothing correlates it.
  """
  @spec encode_event(String.t(), binary(), map()) :: binary()
  def encode_event(topic, body, meta \\ %{}) when is_binary(topic) and is_binary(body) do
    encode_frame(:event, new_request_id(), Map.put(meta, "topic", topic), body)
  end

  @spec encode_frame(frame_type(), binary(), map(), binary()) :: binary()
  def encode_frame(type, request_id, meta, body)
      when is_atom(type) and is_map(meta) and is_binary(body) do
    encode(type_code(type), request_id, meta, body)
  end

  defp encode(type, request_id, meta, body)
       when type in [
              @request_type,
              @response_type,
              @error_type,
              @stream_open_type,
              @stream_data_type,
              @stream_resize_type,
              @stream_close_type,
              @stream_exit_type,
              @stream_error_type,
              @event_type
            ] do
    req_id = ensure_request_id(request_id)
    meta_bytes = IO.iodata_to_binary(:json.encode(meta))

    <<
      @version::8,
      type::8,
      0::16,
      req_id::binary-size(@request_id_bytes),
      byte_size(meta_bytes)::32-big,
      meta_bytes::binary,
      byte_size(body)::32-big,
      body::binary
    >>
  end

  @doc """
  Decode a frame from binary.
  """
  @spec decode(binary()) ::
          {:ok, decoded_frame()} | {:error, :malformed_frame | :unknown_version | :unknown_type}
  def decode(
        <<@version::8, type_code::8, flags::16, request_id::binary-size(@request_id_bytes),
          meta_len::32-big, rest::binary>>
      ) do
    with {:ok, type} <- decode_type(type_code),
         true <- byte_size(rest) >= meta_len + 4,
         <<meta::binary-size(meta_len), body_len::32-big, body::binary>> <- rest,
         true <- byte_size(body) == body_len,
         {:ok, decoded_meta} <- safe_decode_json(meta) do
      {:ok,
       %{
         version: @version,
         type: type,
         flags: flags,
         request_id: request_id,
         meta: decoded_meta,
         body: body
       }}
    else
      false -> {:error, :malformed_frame}
      {:error, _} = error -> error
      _ -> {:error, :malformed_frame}
    end
  end

  def decode(<<version::8, _rest::binary>>) when version != @version do
    {:error, :unknown_version}
  end

  def decode(_), do: {:error, :malformed_frame}

  defp decode_type(@request_type), do: {:ok, :request}
  defp decode_type(@response_type), do: {:ok, :response}
  defp decode_type(@error_type), do: {:ok, :error}
  defp decode_type(@stream_open_type), do: {:ok, :stream_open}
  defp decode_type(@stream_data_type), do: {:ok, :stream_data}
  defp decode_type(@stream_resize_type), do: {:ok, :stream_resize}
  defp decode_type(@stream_close_type), do: {:ok, :stream_close}
  defp decode_type(@stream_exit_type), do: {:ok, :stream_exit}
  defp decode_type(@stream_error_type), do: {:ok, :stream_error}
  defp decode_type(@event_type), do: {:ok, :event}
  defp decode_type(_), do: {:error, :unknown_type}

  defp type_code(:request), do: @request_type
  defp type_code(:response), do: @response_type
  defp type_code(:error), do: @error_type
  defp type_code(:stream_open), do: @stream_open_type
  defp type_code(:stream_data), do: @stream_data_type
  defp type_code(:stream_resize), do: @stream_resize_type
  defp type_code(:stream_close), do: @stream_close_type
  defp type_code(:stream_exit), do: @stream_exit_type
  defp type_code(:stream_error), do: @stream_error_type
  defp type_code(:event), do: @event_type

  defp safe_decode_json(bytes) do
    case :json.decode(bytes) do
      map when is_map(map) -> {:ok, map}
      _ -> {:error, :malformed_frame}
    end
  rescue
    _ -> {:error, :malformed_frame}
  end

  defp ensure_request_id(<<_::binary-size(@request_id_bytes)>> = request_id), do: request_id

  defp ensure_request_id(other) do
    raise ArgumentError,
          "request_id must be #{@request_id_bytes} bytes, got: #{inspect(other)}"
  end
end
