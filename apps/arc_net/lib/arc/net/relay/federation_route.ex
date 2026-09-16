defmodule Arc.Net.Relay.FederationRoute do
  @moduledoc false

  @version 1
  @request 1
  @reply 2
  @max_packet 8 * 1024 * 1024
  @max_record 8_192
  @max_path 9

  def encode(route) when is_map(route) do
    with {:ok, mode} <- mode(Map.get(route, :mode)),
         {:ok, path} <- path(Map.get(route, :path)),
         {:ok, cursor} <- cursor(Map.get(route, :cursor), length(path)),
         {:ok, packet} <- packet(Map.get(route, :packet)),
         {:ok, record} <- record(mode, Map.get(route, :record)) do
      path_bytes = IO.iodata_to_binary(path)

      {:ok,
       <<@version, mode, cursor, length(path), byte_size(packet)::32-big,
         byte_size(record)::16-big, path_bytes::binary, packet::binary, record::binary>>}
    end
  rescue
    _ -> {:error, :invalid_route}
  end

  def encode(_), do: {:error, :invalid_route}

  def decode(
        <<@version, mode_byte, cursor_value, path_len, packet_len::32-big, record_len::16-big,
          rest::binary>>
      ) do
    expected = path_len * 32 + packet_len + record_len

    with {:ok, mode} <- decode_mode(mode_byte),
         true <-
           path_len in 2..@max_path and packet_len <= @max_packet and record_len <= @max_record,
         true <- byte_size(rest) == expected,
         <<path_bytes::binary-size(path_len * 32), packet::binary-size(packet_len),
           record_bytes::binary-size(record_len)>> <- rest,
         {:ok, path} <- decode_path(path_bytes, path_len),
         {:ok, cursor} <- cursor(cursor_value, path_len),
         {:ok, record} <- decode_record(mode, record_bytes) do
      {:ok, %{packet: packet, path: path, cursor: cursor, mode: mode, record: record}}
    else
      _ -> {:error, :invalid_route}
    end
  rescue
    _ -> {:error, :invalid_route}
  end

  def decode(_), do: {:error, :invalid_route}

  defp mode(:request), do: {:ok, @request}
  defp mode(:reply), do: {:ok, @reply}
  defp mode(_), do: {:error, :invalid_route}
  defp decode_mode(@request), do: {:ok, :request}
  defp decode_mode(@reply), do: {:ok, :reply}
  defp decode_mode(_), do: {:error, :invalid_route}

  defp path(path) when is_list(path) and length(path) in 2..@max_path do
    if Enum.all?(path, &match?(<<_::binary-size(32)>>, &1)) and
         MapSet.size(MapSet.new(path)) == length(path),
       do: {:ok, path},
       else: {:error, :invalid_route}
  end

  defp path(_), do: {:error, :invalid_route}

  defp decode_path(bytes, path_length) do
    path = for index <- 0..(path_length - 1), do: binary_part(bytes, index * 32, 32)

    if length(path) == path_length and MapSet.size(MapSet.new(path)) == path_length,
      do: {:ok, path},
      else: {:error, :invalid_route}
  end

  defp cursor(cursor, path_length)
       when is_integer(cursor) and cursor >= 1 and cursor < path_length,
       do: {:ok, cursor}

  defp cursor(_, _), do: {:error, :invalid_route}

  defp packet(packet) when is_binary(packet) and byte_size(packet) <= @max_packet,
    do: {:ok, packet}

  defp packet(_), do: {:error, :invalid_route}

  defp record(@reply, nil), do: {:ok, <<>>}

  defp record(@request, record) when is_map(record) do
    if signed_record_shape?(record) do
      bytes = record |> :json.encode() |> IO.iodata_to_binary()
      if byte_size(bytes) <= @max_record, do: {:ok, bytes}, else: {:error, :invalid_route}
    else
      {:error, :invalid_route}
    end
  rescue
    _ -> {:error, :invalid_route}
  end

  defp record(_, _), do: {:error, :invalid_route}

  defp decode_record(:reply, <<>>), do: {:ok, nil}

  defp decode_record(:request, bytes) when byte_size(bytes) > 0 do
    record = :json.decode(bytes)

    if is_map(record) and signed_record_shape?(record),
      do: {:ok, record},
      else: {:error, :invalid_route}
  rescue
    _ -> {:error, :invalid_route}
  end

  defp decode_record(_, _), do: {:error, :invalid_route}

  defp signed_record_shape?(record) do
    Map.keys(record) |> Enum.all?(&is_binary/1) and
      Enum.all?(
        ~w(version public_key x25519_public capabilities issued_at expires_at signature),
        &Map.has_key?(record, &1)
      )
  end
end
