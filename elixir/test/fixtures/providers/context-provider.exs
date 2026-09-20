#!/usr/bin/env elixir

defmodule ContextProvider do
  def run do
    for line <- IO.stream(:stdio, :line) do
      payload =
        line
        |> String.trim()
        |> :json.decode()

      reply = %{
        "message" => payload["message"],
        "arc_session_id" => payload["arc_session_id"],
        "app_session_id" => payload["app_session_id"],
        "method" => get_in(payload, ["meta", "method"]),
        "custom" => get_in(payload, ["meta", "custom"])
      }

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

ContextProvider.run()
