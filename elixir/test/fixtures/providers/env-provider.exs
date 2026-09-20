#!/usr/bin/env elixir

defmodule EnvProvider do
  def run do
    for line <- IO.stream(:stdio, :line) do
      _payload =
        line
        |> String.trim()
        |> :json.decode()

      reply = %{
        "ARC_HOST_SOCKET" => System.get_env("ARC_HOST_SOCKET"),
        "ARC_HOST_TOKEN" => System.get_env("ARC_HOST_TOKEN"),
        "ARC_IDENTITY" => System.get_env("ARC_IDENTITY"),
        "ARC_IDENTITY_SHORT" => System.get_env("ARC_IDENTITY_SHORT"),
        "ARC_PUBLIC_KEY" => System.get_env("ARC_PUBLIC_KEY")
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

EnvProvider.run()
