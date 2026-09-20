#!/usr/bin/env elixir

defmodule BinaryEchoProvider do
  def run do
    for line <- IO.stream(:stdio, :line) do
      request = line |> String.trim() |> :json.decode()

      emit(%{
        "op" => "reply",
        "request_id" => request["request_id"],
        "encoding" => "base64",
        "reply" => request["message"]
      })
    end
  catch
    :exit, {:terminated, _} -> :ok
    :error, :terminated -> :ok
  end

  defp emit(payload), do: IO.binwrite(IO.iodata_to_binary(:json.encode(payload)) <> "\n")
end

BinaryEchoProvider.run()
