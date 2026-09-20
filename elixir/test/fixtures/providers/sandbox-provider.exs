#!/usr/bin/env elixir

defmodule SandboxProvider do
  def run do
    loop(%{})
  catch
    :exit, {:terminated, _} -> :ok
    :error, :terminated -> :ok
  end

  defp loop(state) do
    case IO.read(:stdio, :line) do
      :eof ->
        :ok

      line ->
        payload =
          line
          |> String.trim()
          |> :json.decode()

        state = handle_event(payload, state)
        loop(state)
    end
  end

  defp handle_event(
         %{"op" => "request", "message" => "NEW " <> image, "request_id" => request_id},
         state
       ) do
    reply = %{"sandbox_id" => "sb-" <> sanitize(image), "image" => image}
    emit(%{"op" => "reply", "request_id" => request_id, "reply" => reply})
    state
  end

  defp handle_event(
         %{
           "op" => "stream_open",
           "message" => "SHELL " <> sandbox_id,
           "app_session_id" => app_session_id
         },
         state
       ) do
    emit(%{
      "op" => "stream_data",
      "app_session_id" => app_session_id,
      "channel" => "stdout",
      "data" => "opened shell for " <> sandbox_id <> "\n"
    })

    Map.put(state, app_session_id, sandbox_id)
  end

  defp handle_event(
         %{"op" => "stream_data", "app_session_id" => app_session_id, "message" => data},
         state
       ) do
    case String.trim(data) do
      "exit" ->
        emit(%{
          "op" => "stream_exit",
          "app_session_id" => app_session_id,
          "status" => 0,
          "message" => "session closed"
        })

        Map.delete(state, app_session_id)

      value ->
        emit(%{
          "op" => "stream_data",
          "app_session_id" => app_session_id,
          "channel" => "stdout",
          "data" => "echo:" <> value <> "\n"
        })

        state
    end
  end

  defp handle_event(
         %{
           "op" => "stream_resize",
           "app_session_id" => app_session_id,
           "cols" => cols,
           "rows" => rows
         },
         state
       ) do
    emit(%{
      "op" => "stream_data",
      "app_session_id" => app_session_id,
      "channel" => "stdout",
      "data" => "resize:" <> to_string(cols) <> "x" <> to_string(rows) <> "\n"
    })

    state
  end

  defp handle_event(%{"op" => "stream_close", "app_session_id" => app_session_id}, state) do
    emit(%{
      "op" => "stream_exit",
      "app_session_id" => app_session_id,
      "status" => 0,
      "message" => "session closed"
    })

    Map.delete(state, app_session_id)
  end

  defp handle_event(_payload, state), do: state

  defp emit(event) do
    IO.puts(IO.iodata_to_binary(:json.encode(event)))
  end

  defp sanitize(image) do
    image
    |> String.downcase()
    |> String.replace(~r/[^a-z0-9]+/u, "-")
    |> String.trim("-")
  end
end

SandboxProvider.run()
