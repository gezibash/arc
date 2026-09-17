defmodule Arc.Net.FederationLifecycleTest do
  use ExUnit.Case, async: false

  alias Arc.Data.{Packet, Session}
  alias Arc.Identity
  alias Arc.Net.{Connection, Federation}

  defmodule CallbackRelay do
    use GenServer
    def init(opts), do: {:ok, Map.new(opts)}

    def handle_call({:manager, pid}, _from, state),
      do: {:reply, :ok, Map.put(state, :manager, pid)}

    def handle_call({:federation_request, _peer, _request}, _from, state) do
      send(state.owner, {:callback_started, self()})

      if state.blocked,
        do: {:noreply, state},
        else: {:reply, %{"origin" => state.label}, state}
    end

    def handle_cast({:federation_frame, conn, peer, payload}, state) do
      Federation.inbound(state.manager, conn, peer, payload)
      {:noreply, state}
    end

    def handle_cast(_, state), do: {:noreply, state}
    def handle_info(_, state), do: {:noreply, state}
  end

  test "the listening relay can make the first application request, and status hides secrets" do
    pair = pair()

    assert {:ok, %{"origin" => "low"}} =
             Federation.request(pair.high, pair.low_id.public_key, %{"type" => "test"})

    status = inspect(:sys.get_status(pair.low), limit: :infinity)
    refute status =~ "secret_key"
    refute status =~ "session_key"
    refute status =~ inspect(pair.low_id.seed)
  end

  test "replaying an encrypted frame closes its authenticated connection" do
    pair = pair()
    link = :sys.get_state(pair.low).links[pair.high_id.public_key]
    plain = <<1, 4, link.channel::binary>>
    {nonce, cipher, seq, session} = Session.encrypt(link.session, plain)

    packet =
      Packet.encode(pair.low_id, pair.high_id.public_key, session.session_id, seq, nonce, cipher,
        ek: session.ek_pub
      )

    monitor = Process.monitor(link.conn)
    assert :ok = Connection.send_federation(link.conn, packet)
    assert :ok = Connection.send_federation(link.conn, packet)
    assert_receive {:DOWN, ^monitor, :process, _, _}, 1_000
    assert Process.alive?(pair.high)
  end

  test "inbound overload closes the peer and terminates its outstanding callback work" do
    pair = pair(blocked: true)
    tasks = for _ <- 1..4, do: Task.async(fn -> request_high(pair) end)

    for _ <- 1..4,
        do: assert_receive({:callback_started, relay} when relay == pair.high_relay, 1_000)

    callbacks = :sys.get_state(pair.high).callbacks |> Map.values()
    assert length(callbacks) == 4
    monitors = Enum.map(callbacks, &Process.monitor(&1.pid))
    overflow = Task.async(fn -> request_high(pair) end)

    for monitor <- monitors do
      assert_receive {:DOWN, ^monitor, :process, _, :shutdown}, 1_000
    end

    for task <- [overflow | tasks], do: assert({:error, _} = Task.await(task, 2_000))
    assert :sys.get_state(pair.high).callbacks == %{}
  end

  test "stopping a manager closes its connections and callback workers" do
    pair = pair(blocked: true)
    request = Task.async(fn -> request_high(pair) end)
    assert_receive {:callback_started, relay} when relay == pair.high_relay, 1_000
    callback = :sys.get_state(pair.high).callbacks |> Map.values() |> hd()
    callback_ref = Process.monitor(callback.pid)
    conn = :sys.get_state(pair.high).links[pair.low_id.public_key].conn
    conn_ref = Process.monitor(conn)
    :ok = GenServer.stop(pair.high, :normal)
    assert_receive {:DOWN, ^callback_ref, :process, _, :shutdown}, 1_000
    assert_receive {:DOWN, ^conn_ref, :process, _, _}, 1_000
    assert {:error, _} = Task.await(request, 2_000)
  end

  test "stopping a manager cancels an in-flight outbound handshake" do
    [identity, peer] =
      Enum.sort_by([Identity.generate(), Identity.generate()], & &1.public_key)

    {:ok, relay} =
      GenServer.start_link(CallbackRelay, owner: self(), label: "dial", blocked: false)

    {:ok, listen} = :gen_tcp.listen(0, [:binary, active: false, reuseaddr: true])
    {:ok, port} = :inet.port(listen)
    owner = self()

    server =
      Task.async(fn ->
        {:ok, socket} = :gen_tcp.accept(listen, 2_000)
        send(owner, :outbound_handshake_accepted)
        :gen_tcp.recv(socket, 0, 2_000)
      end)

    {:ok, federation} =
      Federation.start_link(
        relay: relay,
        identity: identity,
        peers: [%{public_key: peer.public_key, host: ~c"127.0.0.1", port: port}]
      )

    on_exit(fn ->
      stop_process(federation)
      stop_process(relay)
      :gen_tcp.close(listen)
    end)

    Process.unlink(federation)
    Process.unlink(relay)

    assert_receive :outbound_handshake_accepted, 1_000

    await(fn ->
      match?(%{stage: :dialing}, :sys.get_state(federation).links[peer.public_key])
    end)

    dial_pid = :sys.get_state(federation).links[peer.public_key].dial_pid
    dial_monitor = Process.monitor(dial_pid)
    started_at = System.monotonic_time(:millisecond)

    assert :ok = GenServer.stop(federation, :normal, 300)
    assert System.monotonic_time(:millisecond) - started_at < 300
    assert_receive {:DOWN, ^dial_monitor, :process, ^dial_pid, :shutdown}, 1_000
    assert {:error, :closed} = Task.await(server, 1_000)
  end

  defp request_high(pair),
    do: Federation.request(pair.low, pair.high_id.public_key, %{"type" => "test"})

  defp pair(opts \\ []) do
    [low_id, high_id] = Enum.sort_by([Identity.generate(), Identity.generate()], & &1.public_key)

    {:ok, low_relay} =
      GenServer.start_link(CallbackRelay, owner: self(), label: "low", blocked: false)

    {:ok, high_relay} =
      GenServer.start_link(CallbackRelay,
        owner: self(),
        label: "high",
        blocked: Keyword.get(opts, :blocked, false)
      )

    {:ok, listen} = :gen_tcp.listen(0, [:binary, active: false, reuseaddr: true])
    {:ok, port} = :inet.port(listen)

    {:ok, high} =
      Federation.start_link(
        relay: high_relay,
        identity: high_id,
        peers: [%{public_key: low_id.public_key, host: ~c"127.0.0.1", port: 1}]
      )

    :ok = GenServer.call(high_relay, {:manager, high})
    high_key = high_id.public_key

    acceptor =
      Task.async(fn ->
        {:ok, socket} = :gen_tcp.accept(listen, 2_000)

        {:ok, conn} =
          Connection.start_link(
            socket: socket,
            role: :relay_client,
            relay_pid: high_relay,
            relay_pubkey: high_key
          )

        :ok = :gen_tcp.controlling_process(socket, conn)
        Connection.send_relay_hello(conn)
        Connection.activate(conn)
      end)

    {:ok, low} =
      Federation.start_link(
        relay: low_relay,
        identity: low_id,
        peers: [%{public_key: high_id.public_key, host: ~c"127.0.0.1", port: port}]
      )

    :ok = GenServer.call(low_relay, {:manager, low})

    on_exit(fn ->
      Enum.each([low, high, low_relay, high_relay], &stop_process/1)

      :gen_tcp.close(listen)
    end)

    # The on_exit owner shuts these down and verifies their exit. Unlink them
    # from the test process so its automatic shutdown cannot race that cleanup.
    Enum.each([low, high, low_relay, high_relay], &Process.unlink/1)

    await(fn ->
      high_id.public_key in Federation.connected_peers(low) and
        low_id.public_key in Federation.connected_peers(high)
    end)

    Task.await(acceptor, 2_000)
    %{low: low, high: high, low_id: low_id, high_id: high_id, high_relay: high_relay}
  end

  defp await(fun, attempts \\ 100)
  defp await(_fun, 0), do: flunk("federation did not become ready")

  defp await(fun, attempts) do
    unless fun.() do
      receive do
      after
        10 -> :ok
      end

      await(fun, attempts - 1)
    end
  end

  defp stop_process(pid) do
    ref = Process.monitor(pid)

    try do
      GenServer.stop(pid, :normal, 1_000)
    catch
      :exit, {:noproc, _} -> :ok
    end

    assert_receive {:DOWN, ^ref, :process, ^pid, _}, 1_000
  end
end
