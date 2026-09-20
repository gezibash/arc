#!/usr/bin/env elixir

# Replies to every request and also emits one event back to the caller,
# so the CLI's event mode can be tested end to end.
defmodule EventsProvider do
  def run do
    for line <- IO.binstream(:stdio, :line) do
      request = line |> String.trim() |> :json.decode()
      from = Map.get(request, "from", "")

      emit(%{
        "op" => "event",
        "to" => from,
        "topic" => "test.ping",
        "meta" => %{"n" => 1},
        "body" => "pong for " <> from
      })

      emit(%{"op" => "reply", "request_id" => request["request_id"], "reply" => "ok"})
    end
  catch
    :exit, {:terminated, _} -> :ok
    :error, :terminated -> :ok
  end

  defp emit(payload), do: IO.binwrite(IO.iodata_to_binary(:json.encode(payload)) <> "\n")
end

EventsProvider.run()
