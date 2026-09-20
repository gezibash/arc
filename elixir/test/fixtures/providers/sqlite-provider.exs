#!/usr/bin/env elixir

defmodule SqliteProvider do
  def run do
    for line <- IO.stream(:stdio, :line) do
      payload = :json.decode(line)
      sql = payload["message"] |> to_string() |> String.trim()

      reply =
        cond do
          sql == "SELECT 1 AS n" ->
            "n\n-\n1\n1 row(s)"

          String.starts_with?(sql, "SELECT") ->
            "result\n------\n#{sql}\n1 row(s)"

          String.starts_with?(sql, "CREATE") ->
            "OK"

          String.starts_with?(sql, "INSERT") ->
            "OK"

          true ->
            "error: unsupported query"
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

SqliteProvider.run()
