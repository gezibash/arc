defmodule Arc.Host.Client do
  @moduledoc """
  Thin client for the local ARC host service.

  V1 keeps the client simple: open a socket, send one request, read one
  response, close. This is enough for lifecycle commands and request/reply
  SDK-style consumption. Persistent streaming clients can layer on the same
  protocol later.
  """

  @default_timeout_ms 5_000

  def connect(socket_path) when is_binary(socket_path) do
    with {:ok, socket} <- :socket.open(:local, :stream, :default),
         :ok <-
           :socket.connect(socket, %{
             family: :local,
             path: String.to_charlist(socket_path)
           }) do
      {:ok, socket}
    else
      {:error, reason} ->
        {:error, reason}
    end
  end

  def close(socket) do
    :socket.close(socket)
  end

  def request(socket_path, op, params \\ %{}, opts \\ [])
      when is_binary(socket_path) and is_binary(op) and is_map(params) and is_list(opts) do
    with {:ok, socket} <- connect(socket_path),
         result <- request_connected(socket, op, params, opts) do
      _ = close(socket)
      result
    end
  end

  def request_connected(socket, op, params \\ %{}, opts \\ [])
      when is_binary(op) and is_map(params) and is_list(opts) do
    timeout_ms = Keyword.get(opts, :timeout_ms, @default_timeout_ms)
    request_id = Keyword.get(opts, :id, new_request_id())
    params = maybe_put_token(params, Keyword.get(opts, :token))

    with :ok <- send_json(socket, %{"id" => request_id, "op" => op, "params" => params}),
         {:ok, response} <- recv_json_line(socket, timeout_ms) do
      decode_response(response, request_id)
    end
  end

  def recv(socket, timeout_ms \\ @default_timeout_ms) do
    recv_json_line(socket, timeout_ms)
  end

  defp send_json(socket, payload) do
    encoded =
      payload
      |> :json.encode()
      |> IO.iodata_to_binary()

    :socket.send(socket, [encoded, "\n"])
  end

  defp recv_json_line(socket, timeout_ms) do
    recv_json_line(socket, timeout_ms, "")
  end

  defp recv_json_line(socket, timeout_ms, buffer) do
    case String.split(buffer, "\n", parts: 2) do
      [line, rest] ->
        decode_line(line, rest)

      _ ->
        case :socket.recv(socket, 0, timeout_ms) do
          {:ok, data} when is_binary(data) ->
            recv_json_line(socket, timeout_ms, buffer <> data)

          {:error, reason} ->
            {:error, reason}
        end
    end
  end

  defp decode_line("", _rest), do: {:error, :invalid_response}

  defp decode_line(line, _rest) do
    try do
      {:ok, :json.decode(line)}
    rescue
      _ -> {:error, :invalid_json}
    end
  end

  defp decode_response(%{"id" => id, "ok" => true, "result" => result}, id), do: {:ok, result}

  defp decode_response(%{"id" => id, "ok" => false, "error" => error}, id) when is_map(error) do
    {:error, {error["code"] || "error", error["message"] || "unknown error"}}
  end

  defp decode_response(_response, _request_id), do: {:error, :unexpected_response}

  defp new_request_id do
    8
    |> :crypto.strong_rand_bytes()
    |> Base.encode16(case: :lower)
  end

  defp maybe_put_token(params, nil), do: params
  defp maybe_put_token(params, ""), do: params
  defp maybe_put_token(params, token), do: Map.put_new(params, "token", token)
end
