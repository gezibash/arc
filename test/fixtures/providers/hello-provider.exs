#!/usr/bin/env elixir

defmodule HelloProvider do
  def run do
    for line <- IO.stream(:stdio, :line) do
      request =
        line
        |> String.trim()
        |> :json.decode()

      message = Map.get(request, "message", "")
      from = Map.get(request, "from", "")

      reply =
        if String.starts_with?(message, "POST /echo ") do
          String.replace_prefix(message, "POST /echo ", "")
        else
          "provider hello from " <> from <> ": " <> message
        end

      emit(%{"reply" => reply})
    end
  catch
    :exit, {:terminated, _} -> :ok
    :error, :terminated -> :ok
  end

  defp emit(payload) do
    IO.binwrite(IO.iodata_to_binary(:json.encode(payload)) <> "\n")
  end
end

HelloProvider.run()
