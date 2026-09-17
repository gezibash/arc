defmodule Arc.CLI.Update.ObserverTest do
  use ExUnit.Case, async: false

  alias Arc.CLI.Update.Observer
  alias Arc.Net.Relay

  setup do
    Application.ensure_all_started(:arc_net)
    {:ok, relay} = Relay.start_link(0)

    on_exit(fn -> stop_relay(relay) end)

    %{relay: relay}
  end

  test "observes encrypted bidirectional relay traffic without replacing runtime", %{relay: relay} do
    assert {:ok, observer} = Observer.start(relay)
    Process.sleep(125)

    assert {:ok, result} = Observer.finish(observer)
    assert result["verdict"] == "passed"
    assert result["port"] == Relay.get_port(relay)
    assert result["messages"]["alice_to_bob"] >= 2
    assert result["messages"]["bob_to_alice"] >= 2
    assert result["runtime"]["tracked_connections"] >= 2
    assert is_binary(IO.iodata_to_binary(:json.encode(result)))
    refute Process.alive?(observer)
  end

  test "returns a probe failure when a disposable socket is lost", %{relay: relay} do
    assert {:ok, observer} = Observer.start(relay)
    alice_socket = :sys.get_state(observer).alice_socket
    :ok = :gen_tcp.close(alice_socket)

    assert eventually(fn -> :sys.get_state(observer).failure != nil end)
    assert {:error, {:probe_failed, {:alice_to_bob, _}}} = Observer.finish(observer)
    refute Process.alive?(observer)
  end

  test "abort closes a running observation", %{relay: relay} do
    assert {:ok, observer} = Observer.start(relay)
    assert :ok = Observer.abort(observer)
    refute Process.alive?(observer)
  end

  test "rejects a non-process relay target" do
    assert {:error, :invalid_relay} = Observer.start(nil)
  end

  test "owner exit closes the probe instead of leaving background traffic", %{relay: relay} do
    parent = self()

    owner =
      spawn(fn ->
        {:ok, observer} = Observer.start(relay)
        send(parent, {:observer, observer})

        receive do
          :stop -> :ok
        end
      end)

    assert_receive {:observer, observer}, 3_000
    ref = Process.monitor(observer)
    send(owner, :stop)
    assert_receive {:DOWN, ^ref, :process, ^observer, :normal}, 2_000
  end

  defp eventually(fun, attempts \\ 40)

  defp eventually(_fun, 0), do: false

  defp eventually(fun, attempts) do
    if fun.() do
      true
    else
      Process.sleep(25)
      eventually(fun, attempts - 1)
    end
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
end
