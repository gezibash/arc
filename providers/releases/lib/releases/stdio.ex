defmodule Releases.Stdio do
  @moduledoc false

  alias Releases.Store
  @max_line_bytes 512 * 1024

  def loop(root, device \\ :stdio) do
    :io.setopts(:standard_io, encoding: :latin1)
    IO.binstream(device, :line) |> Enum.each(&handle_line(root, &1))
  end

  def handle_line(root, line) when byte_size(line) <= @max_line_bytes do
    case decode(String.trim_trailing(line, "\n")) do
      {:ok, %{"op" => "request", "message" => message} = event} when is_binary(message) ->
        case request(root, message) do
          {:ok, reply} ->
            emit(%{"op" => "reply", "request_id" => event["request_id"], "reply" => reply})

          {:error, reason} ->
            emit(%{"op" => "error", "request_id" => event["request_id"], "error" => reason})
        end

      {:ok, %{"op" => op, "app_session_id" => sid}} when op in ["stream_open", "stream_data"] ->
        emit(%{
          "op" => "stream_error",
          "app_session_id" => sid,
          "message" => "streams not supported"
        })

      _ ->
        emit(%{"op" => "error", "request_id" => nil, "error" => "invalid_request"})
    end
  end

  def handle_line(_, _),
    do: emit(%{"op" => "error", "request_id" => nil, "error" => "invalid_request"})

  defp request(root, message) do
    case decode(message) do
      {:ok, %{"op" => "channel", "channel" => channel}} ->
        Store.channel(root, channel)

      {:ok, %{"op" => "chunk", "digest" => digest, "offset" => offset, "length" => length}} ->
        Store.chunk(root, digest, offset, length)

      _ ->
        {:error, "invalid_request"}
    end
  end

  defp decode(body) do
    {:ok, :json.decode(body)}
  rescue
    _ -> :error
  end

  defp emit(map), do: IO.binwrite(:stdio, [IO.iodata_to_binary(:json.encode(map)), "\n"])
end
