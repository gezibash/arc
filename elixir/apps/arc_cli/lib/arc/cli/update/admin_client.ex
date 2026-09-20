defmodule Arc.CLI.Update.AdminClient do
  @moduledoc false

  @max_response_bytes 64 * 1024
  @default_timeout_ms 5_000

  def request(socket_path, op, params \\ %{}, opts \\ [])
      when is_binary(socket_path) and op in ["status", "check", "apply"] and is_map(params) do
    timeout = Keyword.get(opts, :timeout_ms, @default_timeout_ms)
    deadline = System.monotonic_time(:millisecond) + timeout

    with {:ok, socket} <- connect(socket_path, remaining(deadline)) do
      result =
        with :ok <- send_request(socket, op, params, remaining(deadline)) do
          recv_response(socket, deadline, "")
        end

      _ = :socket.close(socket)
      result
    end
  end

  defp connect(_socket_path, remaining) when remaining <= 0, do: {:error, :timeout}

  defp connect(socket_path, remaining) do
    with {:ok, socket} <- :socket.open(:local, :stream, :default) do
      case :socket.connect(
             socket,
             %{family: :local, path: String.to_charlist(socket_path)},
             remaining
           ) do
        :ok ->
          {:ok, socket}

        {:error, reason} ->
          _ = :socket.close(socket)
          {:error, reason}
      end
    end
  end

  defp send_request(_socket, _op, _params, remaining) when remaining <= 0, do: {:error, :timeout}

  defp send_request(socket, op, params, remaining) do
    :socket.send(
      socket,
      [IO.iodata_to_binary(:json.encode(%{"op" => op, "params" => params})), "\n"],
      remaining
    )
  end

  defp recv_response(_socket, _deadline, buffer) when byte_size(buffer) > @max_response_bytes,
    do: {:error, :response_too_large}

  defp recv_response(socket, deadline, buffer) do
    case String.split(buffer, "\n", parts: 2) do
      [line, _rest] when byte_size(line) > 0 ->
        decode_response(line)

      _ ->
        remaining = deadline - System.monotonic_time(:millisecond)

        if remaining <= 0 do
          {:error, :timeout}
        else
          case :socket.recv(socket, 0, remaining) do
            {:ok, data} when is_binary(data) -> recv_response(socket, deadline, buffer <> data)
            {:error, reason} -> {:error, reason}
          end
        end
    end
  end

  defp decode_response(line) do
    case :json.decode(line) do
      %{"ok" => true, "result" => result} when is_map(result) -> {:ok, result}
      %{"ok" => false, "error" => reason} when is_binary(reason) -> {:error, reason}
      _ -> {:error, :invalid_response}
    end
  rescue
    _ -> {:error, :invalid_json}
  end

  defp remaining(deadline), do: deadline - System.monotonic_time(:millisecond)
end
