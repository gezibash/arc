defmodule Arc.Net.LoadTest do
  use ExUnit.Case, async: false

  setup do
    Application.ensure_all_started(:arc_net)
    {:ok, relay} = Arc.Net.Relay.start_link(0)
    port = Arc.Net.Relay.get_port(relay)

    on_exit(fn ->
      stop_relay(relay)
    end)

    %{port: port}
  end

  defp stop_relay(relay) do
    if Process.alive?(relay) do
      Process.unlink(relay)

      try do
        GenServer.stop(relay, :normal)
      catch
        :exit, _ -> :ok
      end
    end
  end

  test "run/1 opens requested connections", %{port: port} do
    {:ok, summary} =
      Arc.Net.Load.run(
        host: "localhost",
        port: port,
        connections: 8,
        packets: 0,
        timeout_ms: 1000
      )

    assert summary.connections_requested == 8
    assert summary.connections_opened == 8
    assert summary.connect_errors == 0
    assert summary.packets_sent == 0
  end

  test "run/1 sends packet pressure in drop mode", %{port: port} do
    {:ok, summary} =
      Arc.Net.Load.run(
        host: "localhost",
        port: port,
        connections: 10,
        packets: 30,
        mode: :drop,
        payload_bytes: 48,
        timeout_ms: 1000
      )

    assert summary.connections_opened == 10
    assert summary.packets_requested == 30
    assert summary.packets_sent == 30
    assert summary.send_errors == 0
  end
end
