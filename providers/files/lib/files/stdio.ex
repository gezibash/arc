defmodule Files.Stdio do
  @moduledoc false
  def loop(root, device \\ :stdio) do
    :io.setopts(:standard_io, encoding: :latin1)
    IO.binstream(device, :line) |> Enum.each(&handle_line(root, &1))
  end

  def handle_line(root, line) do
    if byte_size(line) > Files.Config.max_line_bytes() do
      emit_error(nil, "invalid_request")
    else
      do_handle_line(root, line)
    end
  end

  defp do_handle_line(root, line) do
    case decode(String.trim_trailing(line, "\n")) do
      {:ok, %{"op" => "request", "from" => from, "message" => message} = event}
      when is_binary(from) and is_binary(message) ->
        case Files.Command.run(root, from, message) do
          {:ok, response} -> emit_reply(event["request_id"], response)
          {:error, reason} -> emit_error(event["request_id"], reason)
        end

      {:ok, %{"op" => op, "app_session_id" => sid}} when op in ["stream_open", "stream_data"] ->
        emit_raw(%{
          "op" => "stream_error",
          "app_session_id" => sid,
          "message" => "streams not supported"
        })

      _ ->
        emit_error(nil, "invalid_request")
    end
  end

  defp decode(line) do
    {:ok, :json.decode(line)}
  rescue
    _ -> :error
  end

  # The exec runtime serializes the object. Pre-encoding this value would
  # turn :json's iodata into a nested JSON array on the wire.
  defp emit_reply(id, reply),
    do: emit_raw(%{"op" => "reply", "request_id" => id, "reply" => reply})

  defp emit_error(id, reason),
    do: emit_raw(%{"op" => "error", "request_id" => id, "error" => reason})

  defp emit_raw(map), do: IO.binwrite(:stdio, [IO.iodata_to_binary(:json.encode(map)), "\n"])
end
