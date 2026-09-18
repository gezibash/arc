defmodule Arc.Net.Relay.CatalogRuntimeTest do
  use ExUnit.Case, async: false

  alias Arc.Data.RelayAnnouncement
  alias Arc.Identity
  alias Arc.Net.{Handshake, Relay}
  alias Arc.Net.Relay.FederationCatalog, as: Catalog

  defmodule Manager do
    use GenServer
    def start_link(owner), do: GenServer.start_link(__MODULE__, owner)
    def init(owner), do: {:ok, owner}

    def handle_call({:request, peer, request, _timeout}, from, owner) do
      send(owner, {:request, peer, request, from})
      {:noreply, owner}
    end
  end

  setup do
    identities = Enum.sort_by(for(_ <- 1..6, do: Identity.generate()), & &1.public_key)
    home = List.last(identities)
    peers = Enum.drop(identities, -1)

    {:ok, relay} =
      Relay.start_link(0,
        relay_identity: home,
        federation_peers:
          Enum.map(peers, &%{public_key: &1.public_key, host: ~c"127.0.0.1", port: 1})
      )

    on_exit(fn -> Arc.Net.TestTeardown.stop(relay) end)
    manager = start_supervised!({Manager, self()})
    :sys.replace_state(relay, &%{&1 | federation: manager})

    %{
      relay: relay,
      home: home.public_key,
      peers: Enum.map(peers, & &1.public_key),
      port: Relay.get_port(relay)
    }
  end

  test "ready peers synchronize without searches; warm searches and exact identities stay local",
       ctx do
    peer = hd(ctx.peers)
    provider = Identity.generate()

    record =
      RelayAnnouncement.create(provider, [%{"id" => "files"}],
        federation: :network,
        relay_public_key: peer
      )

    {:ok, entry} = RelayAnnouncement.verify(record)
    source = Catalog.new(peer, [ctx.home]) |> Catalog.rebuild([entry], false)

    send(ctx.relay, {:federation_peer_up, peer})
    assert_receive {:request, ^peer, %{"type" => "catalog"} = request, from}, 1_000
    {reply, source} = Catalog.request(source, ctx.home, request)
    GenServer.reply(from, {:ok, reply})
    await(fn -> Relay.stats(ctx.relay).catalog_synced_peers == 1 end)

    socket = connect(ctx, Identity.generate())

    for query <- ["files", "files", "not-offered"] do
      assert %{"ok" => true, "cached" => true, "partial" => true} =
               directory(socket, %{"type" => "search", "query" => query})
    end

    assert %{"cached" => true, "entries" => [^record]} =
             directory(socket, %{"type" => "resolve", "query" => record["public_key"]})

    assert Relay.stats(ctx.relay).federation_live_queries == 0

    # Provider withdrawal reaches the relay through background delta polling.
    source = Catalog.rebuild(source, [], false)
    send(ctx.relay, :sync_catalogs)
    assert_receive {:request, ^peer, %{"mode" => "delta"} = request, from}, 1_000
    {reply, _} = Catalog.request(source, ctx.home, request)
    GenServer.reply(from, {:ok, reply})
    await(fn -> Relay.stats(ctx.relay).catalog_sync_pages == 2 end)

    assert %{"cached" => true, "entries" => []} =
             directory(socket, %{"type" => "search", "query" => "files"})

    assert Relay.stats(ctx.relay).federation_routes == 0
    assert Relay.stats(ctx.relay).federation_live_queries == 0
  end

  test "catalog jobs are bounded and disconnected peers cannot install late pages", ctx do
    Enum.each(ctx.peers, &send(ctx.relay, {:federation_peer_up, &1}))

    jobs =
      for _ <- 1..4 do
        assert_receive {:request, peer, %{"type" => "catalog"} = request, from}, 1_000
        {peer, request, from}
      end

    assert Relay.stats(ctx.relay).catalog_syncs == 4
    refute_receive {:request, _, _, _}, 50
    {peer, request, from} = hd(jobs)
    old = :sys.get_state(ctx.relay).catalog_syncs[peer]
    monitor = Process.monitor(old.pid)
    send(ctx.relay, {:federation_peer_down, peer})
    assert_receive {:DOWN, ^monitor, :process, _, :shutdown}, 1_000
    assert Relay.stats(ctx.relay).catalog_syncs == 3

    source = Catalog.new(peer, [ctx.home]) |> Catalog.rebuild([], false)
    {reply, _} = Catalog.request(source, ctx.home, request)
    GenServer.reply(from, {:ok, reply})
    send(ctx.relay, {:catalog_result, peer, old.token, {:ok, reply}})
    assert Relay.stats(ctx.relay).catalog_synced_peers == 0
    refute MapSet.member?(:sys.get_state(ctx.relay).catalog_ready, peer)

    # Finishing another job must schedule the peer that has not had a turn.
    {other, req, waiting} = Enum.at(jobs, 1)
    {reply, _} = Catalog.request(Catalog.new(other, [ctx.home]), ctx.home, req)
    GenServer.reply(waiting, {:ok, reply})
    unseen = ctx.peers -- Enum.map(jobs, &elem(&1, 0))
    assert_receive {:request, next, %{"type" => "catalog"}, _}, 1_000
    assert next in unseen
  end

  test "an earlier live result cannot reinstall a route after a catalog change", ctx do
    peer = hd(ctx.peers)
    provider = Identity.generate()

    record =
      RelayAnnouncement.create(provider, [%{"id" => "files"}],
        federation: :network,
        relay_public_key: peer
      )

    {:ok, entry} = RelayAnnouncement.verify(record)
    source = Catalog.new(peer, [ctx.home]) |> Catalog.rebuild([entry], false)
    socket = connect(ctx, Identity.generate())
    send_directory(socket, %{"type" => "search", "query" => "files"})

    pending =
      for _ <- ctx.peers do
        assert_receive {:request, key, %{"type" => "search"}, from}, 1_000
        {key, from}
      end

    send(ctx.relay, {:federation_peer_up, peer})
    assert_receive {:request, ^peer, %{"type" => "catalog"} = req, from}, 1_000
    {reply, source} = Catalog.request(source, ctx.home, req)
    GenServer.reply(from, {:ok, reply})
    await(fn -> Relay.stats(ctx.relay).catalog_synced_peers == 1 end)
    source = Catalog.rebuild(source, [], false)
    send(ctx.relay, :sync_catalogs)
    assert_receive {:request, ^peer, %{"type" => "catalog"} = req, from}, 1_000
    {reply, _} = Catalog.request(source, ctx.home, req)
    GenServer.reply(from, {:ok, reply})
    await(fn -> Relay.stats(ctx.relay).catalog_sync_pages == 2 end)

    for {key, from} <- pending do
      entries = if key == peer, do: [record], else: []
      routes = if key == peer, do: %{record["public_key"] => [hex(peer)]}, else: %{}

      GenServer.reply(
        from,
        {:ok, %{"entries" => entries, "routes" => routes, "next" => :null, "partial" => false}}
      )
    end

    assert %{"cached" => true, "entries" => []} = receive_directory(socket)
    assert Relay.stats(ctx.relay).federation_routes == 0

    assert %{"cached" => true, "entries" => []} =
             directory(socket, %{"type" => "search", "query" => "files"})
  end

  defp connect(ctx, identity) do
    {:ok, socket} = :gen_tcp.connect(~c"127.0.0.1", ctx.port, [:binary, active: false])
    on_exit(fn -> :gen_tcp.close(socket) end)
    {:ok, hello} = :gen_tcp.recv(socket, 64, 1_000)
    {:ok, key, challenge} = Handshake.decode_relay_hello(hello)
    _info = recv(socket)
    {:ok, proof, _} = Handshake.client_hello(identity, key, challenge)
    :ok = :gen_tcp.send(socket, proof)
    socket
  end

  defp directory(socket, request) do
    send_directory(socket, request)
    receive_directory(socket)
  end

  defp send_directory(socket, request) do
    control = Map.put(request, "request_id", hex(:crypto.strong_rand_bytes(16)))
    body = "ARC_DIRECTORY_V1" <> IO.iodata_to_binary(:json.encode(control))
    :ok = :gen_tcp.send(socket, <<byte_size(body)::32-big, body::binary>>)
  end

  defp receive_directory(socket) do
    <<"ARC_DIRECTORY_V1", payload::binary>> = recv(socket)
    :json.decode(payload)
  end

  defp recv(socket) do
    {:ok, <<size::32-big>>} = :gen_tcp.recv(socket, 4, 1_000)
    {:ok, body} = :gen_tcp.recv(socket, size, 1_000)
    body
  end

  defp await(check, remaining \\ 100)

  defp await(check, remaining) when remaining > 0 do
    if check.(),
      do: :ok,
      else:
        (
          Process.sleep(10)
          await(check, remaining - 1)
        )
  end

  defp await(_check, 0), do: flunk("catalog did not synchronize")
  defp hex(key), do: Base.encode16(key, case: :lower)
end
