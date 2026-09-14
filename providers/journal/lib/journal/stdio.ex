defmodule Journal.Stdio do
  @moduledoc """
  Newline-delimited JSON over stdin/stdout, the ARC exec runtime protocol.

  In:  {"op":"request","message":"...","from":"<hex pk>","request_id":"..."}
  Out: {"op":"reply","request_id":"...","reply":"..."}
       {"op":"error","request_id":"...","error":"..."}
  """

  @doc """
  Reads request lines from `device` until it closes. Lines are read as raw
  bytes: the JSON is UTF-8 on the wire, and `IO.stream` on the escript's
  latin1 stdio would re-encode every byte as a code point.
  """
  def loop(root, device \\ :stdio) do
    :io.setopts(:standard_io, encoding: :latin1)

    IO.binstream(device, :line)
    |> Stream.map(&String.trim_trailing(&1, "\n"))
    |> Stream.reject(&(&1 == ""))
    |> Enum.each(&handle_line(root, &1))
  end

  def handle_line(root, line) do
    case decode(line) do
      {:ok, %{"op" => "request"} = event} ->
        emit(run(root, event), event["request_id"])

      {:ok, %{"op" => op, "app_session_id" => sid}} when op in ~w(stream_open stream_data) ->
        emit_raw(%{
          "op" => "stream_error",
          "app_session_id" => sid,
          "message" => "streams not supported"
        })

      {:ok, _other} ->
        :ok

      :error ->
        emit({:error, "invalid request"}, nil)
    end
  end

  defp run(root, event) do
    from = event["from"] || ""
    message = event["message"] || ""
    Journal.Command.run(root, from, message)
  rescue
    e -> {:error, "internal: " <> Exception.message(e)}
  end

  defp decode(line) do
    {:ok, :json.decode(line)}
  rescue
    _ -> :error
  end

  defp emit({:ok, reply}, request_id),
    do: emit_raw(%{"op" => "reply", "request_id" => request_id, "reply" => reply})

  defp emit({:error, reason}, request_id),
    do: emit_raw(%{"op" => "error", "request_id" => request_id, "error" => reason})

  defp emit_raw(map) do
    IO.binwrite(:stdio, [IO.iodata_to_binary(:json.encode(map)), "\n"])
  end
end
