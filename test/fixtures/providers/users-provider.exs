#!/usr/bin/env elixir

defmodule UsersProvider do
  def run do
    for line <- IO.stream(:stdio, :line) do
      request =
        line
        |> String.trim()
        |> :json.decode()

      message = Map.get(request, "message", "")

      reply =
        cond do
          message == "GET /users" ->
            ~s([{"id":1,"name":"Ada","email":"ada@example.com"}])

          String.starts_with?(message, "POST /users ") ->
            String.replace_prefix(message, "POST /users ", "")

          true ->
            ~s({"error":"unknown request"})
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

UsersProvider.run()
