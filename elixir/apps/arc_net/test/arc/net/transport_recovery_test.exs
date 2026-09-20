defmodule Arc.Net.TransportRecoveryTest do
  use ExUnit.Case, async: false

  alias Arc.Identity
  alias Arc.Net.Relay
  alias Arc.Net.Transport
  alias Arc.Net.TransportManager

  setup do
    Application.ensure_all_started(:arc_net)

    original =
      for key <- [:relay_reconnect_base_ms, :relay_reconnect_cap_ms, :relay_connect_timeout_ms],
          into: %{},
          do: {key, Application.get_env(:arc_net, key)}

    Application.put_env(:arc_net, :relay_reconnect_base_ms, 10)
    Application.put_env(:arc_net, :relay_reconnect_cap_ms, 20)
    Application.put_env(:arc_net, :relay_connect_timeout_ms, 100)

    on_exit(fn ->
      Enum.each(original, fn {key, value} ->
        if value == nil,
          do: Application.delete_env(:arc_net, key),
          else: Application.put_env(:arc_net, key, value)
      end)
    end)

    :ok
  end

  test "reconnects only to the previous relay identity after a drop" do
    relay_key = :crypto.strong_rand_bytes(32)
    {:ok, relay} = Relay.start_link(0, relay_public_key: relay_key)
    port = Relay.get_port(relay)
    identity = Identity.generate()
    {:ok, transport} = Transport.start_link()

    on_exit(fn ->
      stop(transport)
      stop(relay)
    end)

    assert :ok = Transport.connect_relay(transport, ~c"localhost", port, identity, relay_key)
    assert {:ok, ^relay_key} = Transport.relay_public_key(transport)

    stop(relay)
    drop_connection(transport)

    assert eventually(fn ->
             Transport.relay_public_key(transport) == {:error, :relay_not_connected}
           end)

    # A different relay on the same address cannot satisfy the saved pin.
    wrong_key = :crypto.strong_rand_bytes(32)
    {:ok, wrong_relay} = Relay.start_link(port, relay_public_key: wrong_key)

    assert eventually(fn ->
             Transport.relay_public_key(transport) == {:error, :relay_not_connected}
           end)

    stop(wrong_relay)

    {:ok, restored} = Relay.start_link(port, relay_public_key: relay_key)
    assert eventually(fn -> Transport.relay_public_key(transport) == {:ok, relay_key} end)
    stop(restored)
  end

  test "reports an existing relay connection and its bounded reconnect state" do
    relay_key = :crypto.strong_rand_bytes(32)
    {:ok, relay} = Relay.start_link(0, relay_public_key: relay_key)
    port = Relay.get_port(relay)
    identity = Identity.generate()
    {:ok, idle_transport} = Transport.start_link()

    on_exit(fn ->
      stop(idle_transport)
      TransportManager.reset()
      stop(relay)
    end)

    assert {:ok, %{status: :disconnected}} = Transport.relay_status(idle_transport)

    assert :ok = Arc.Net.connect_relay(~c"localhost", port, identity, relay_key)

    assert {:ok, %{status: :connected, host: "localhost", port: ^port}} =
             Arc.Net.relay_status(identity.public_key)

    assert {:ok, transport} = TransportManager.lookup(identity.public_key)

    assert :ok = :sys.suspend(TransportManager)

    try do
      {elapsed_us, result} = :timer.tc(fn -> Arc.Net.relay_status(identity.public_key) end)
      assert result == {:error, :relay_status_unavailable}
      assert elapsed_us < 1_000_000
    after
      :ok = :sys.resume(TransportManager)
    end

    assert :ok = :sys.suspend(transport)

    try do
      {elapsed_us, result} = :timer.tc(fn -> Arc.Net.relay_status(identity.public_key) end)
      assert result == {:error, :relay_status_unavailable}
      assert elapsed_us < 1_000_000
    after
      :ok = :sys.resume(transport)
    end

    assert {:error, :relay_not_connected} = Arc.Net.relay_status(Identity.generate().public_key)

    stop(relay)
    drop_connection(transport)

    assert eventually(fn ->
             Arc.Net.relay_status(identity.public_key) ==
               {:ok, %{status: :reconnecting, host: "localhost", port: port}}
           end)
  end

  test "an explicit switch cancels a pending retry for the old relay" do
    first_key = :crypto.strong_rand_bytes(32)
    {:ok, first} = Relay.start_link(0, relay_public_key: first_key)
    first_port = Relay.get_port(first)
    identity = Identity.generate()
    {:ok, transport} = Transport.start_link()

    on_exit(fn ->
      stop(transport)
      stop(first)
    end)

    assert :ok =
             Transport.connect_relay(transport, ~c"localhost", first_port, identity, first_key)

    stop(first)
    drop_connection(transport)

    assert eventually(fn ->
             Transport.relay_public_key(transport) == {:error, :relay_not_connected}
           end)

    second_key = :crypto.strong_rand_bytes(32)
    {:ok, second} = Relay.start_link(0, relay_public_key: second_key)

    on_exit(fn -> stop(second) end)

    assert :ok =
             Transport.connect_relay(
               transport,
               ~c"localhost",
               Relay.get_port(second),
               identity,
               second_key
             )

    assert eventually(fn -> Transport.relay_public_key(transport) == {:ok, second_key} end)

    state = :sys.get_state(transport)
    assert state.relay_target == {~c"localhost", Relay.get_port(second)}
    assert state.relay_config.pin == second_key
    assert state.reconnect_timer == nil
  end

  test "manager removes a terminated transport without crashing" do
    # The manager is global. Entries from an earlier test module must not count here.
    :ok = TransportManager.reset()
    {:ok, relay} = Relay.start_link(0)
    identity = Identity.generate()

    on_exit(fn ->
      TransportManager.reset()
      stop(relay)
    end)

    assert :ok = Arc.Net.connect_relay(~c"localhost", Relay.get_port(relay), identity)
    assert {:ok, transport} = TransportManager.lookup(identity.public_key)

    assert :ok = DynamicSupervisor.terminate_child(Arc.Net.TransportSupervisor, transport)
    assert eventually(fn -> TransportManager.lookup(identity.public_key) == :error end)

    # lookup/1 reports :error for a dead transport before the manager handles
    # its :DOWN message, so wait for the entry itself to go.
    assert eventually(fn -> TransportManager.count() == 0 end)
  end

  defp eventually(fun, attempts \\ 80)

  defp eventually(fun, attempts) when attempts > 0 do
    if fun.(),
      do: true,
      else:
        (
          Process.sleep(25)
          eventually(fun, attempts - 1)
        )
  end

  defp eventually(_fun, 0), do: false

  defp drop_connection(transport) do
    conn = :sys.get_state(transport).relay_conn
    # The connection can already be gone, because it closes when its relay stops.
    if is_pid(conn), do: Process.exit(conn, :shutdown)
  end

  defp stop(pid) when is_pid(pid) do
    if Process.alive?(pid) do
      Process.unlink(pid)

      try do
        GenServer.stop(pid, :normal)
      catch
        :exit, _ -> :ok
      end
    end
  end
end
