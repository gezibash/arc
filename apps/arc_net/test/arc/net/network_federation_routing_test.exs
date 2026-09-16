defmodule Arc.Net.NetworkFederationRoutingTest do
  use ExUnit.Case, async: false

  alias Arc.Data.{Packet, RelayAnnouncement}
  alias Arc.Identity
  alias Arc.Net.{Handshake, Relay}

  defmodule Forwarder do
    use GenServer
    def start_link(owner), do: GenServer.start_link(__MODULE__, owner)
    def init(owner), do: {:ok, owner}

    def handle_call({:forward_route, peer, route}, _from, owner) do
      send(owner, {:forwarded, peer, route})
      {:reply, :ok, owner}
    end

    def handle_call({:request, peer, request, _timeout}, from, owner) do
      send(owner, {:directory_query, peer, request, from})
      {:noreply, owner}
    end
  end

  setup do
    [left, right, home] = Enum.sort_by(for(_ <- 1..3, do: Identity.generate()), & &1.public_key)

    {:ok, relay} =
      Relay.start_link(0,
        relay_identity: home,
        federation_peers:
          for(
            peer <- [left, right],
            do: %{public_key: peer.public_key, host: ~c"127.0.0.1", port: 1}
          )
      )

    on_exit(fn -> if Process.alive?(relay), do: GenServer.stop(relay, :normal) end)
    sink = start_supervised!({Forwarder, self()})
    :sys.replace_state(relay, &%{&1 | federation: sink})
    %{relay: relay, home: home, left: left, right: right, port: Relay.get_port(relay)}
  end

  test "onward traffic requires the operator's explicit opt-in", ctx do
    route = transit_request(ctx)
    Relay.federation_routed_packet(ctx.relay, ctx.left.public_key, route)
    assert :sys.get_state(ctx.relay).federation_network_returns == %{}
    refute_receive {:forwarded, _, _}, 50
  end

  test "transit preserves ciphertext and replies must match the complete conversation path",
       ctx do
    :sys.replace_state(ctx.relay, &%{&1 | federation_transit: true})
    provider = Identity.generate()
    citizen = Identity.generate()
    sid = :crypto.strong_rand_bytes(16)
    route = transit_request(ctx, provider, citizen, sid)
    Relay.federation_routed_packet(ctx.relay, ctx.left.public_key, route)
    assert_receive {:forwarded, next, forwarded}
    assert next == ctx.right.public_key
    assert forwarded == %{route | cursor: 2}

    reply = %{
      route
      | path: Enum.reverse(route.path),
        mode: :reply,
        record: nil,
        packet: packet(provider, citizen.public_key, sid)
    }

    Relay.federation_routed_packet(ctx.relay, ctx.left.public_key, reply)

    Relay.federation_routed_packet(ctx.relay, ctx.right.public_key, %{
      reply
      | packet: packet(provider, citizen.public_key)
    })

    refute_receive {:forwarded, _, _}, 50

    Relay.federation_routed_packet(ctx.relay, ctx.right.public_key, reply)
    assert_receive {:forwarded, previous, returned}
    assert previous == ctx.left.public_key
    assert returned == %{reply | cursor: 2}
    refute Map.has_key?(:sys.get_state(ctx.relay).directory_records, citizen.public_key)

    send(ctx.relay, {:federation_peer_down, ctx.right.public_key})
    assert :sys.get_state(ctx.relay).federation_network_returns == %{}
    Relay.federation_routed_packet(ctx.relay, ctx.right.public_key, reply)
    refute_receive {:forwarded, _, _}, 50
  end

  test "transit rejects direct or private scopes, false homes, loops, and wrong ingress", ctx do
    :sys.replace_state(ctx.relay, &%{&1 | federation_transit: true})
    provider = Identity.generate()
    route = transit_request(ctx, provider)

    invalid = [
      %{route | record: RelayAnnouncement.create(provider, [])},
      %{
        route
        | record:
            RelayAnnouncement.create(provider, [],
              federation: :direct,
              relay_public_key: ctx.right.public_key
            )
      },
      %{
        route
        | record:
            RelayAnnouncement.create(provider, [],
              federation: :network,
              relay_public_key: ctx.left.public_key
            )
      },
      %{route | path: [ctx.left.public_key, ctx.home.public_key, ctx.left.public_key]},
      %{route | cursor: 2},
      %{route | record: Map.put(route.record, "federation", "direct")}
    ]

    for candidate <- invalid,
        do: Relay.federation_routed_packet(ctx.relay, ctx.left.public_key, candidate)

    Relay.federation_routed_packet(ctx.relay, Identity.generate().public_key, route)
    assert :sys.get_state(ctx.relay).federation_network_returns == %{}
    refute_receive {:forwarded, _, _}, 50
  end

  test "the destination rechecks current sharing even after creating a return permission", ctx do
    provider = Identity.generate()
    citizen = Identity.generate()
    socket = connect(ctx, provider)

    record =
      RelayAnnouncement.create(provider, [],
        federation: :network,
        relay_public_key: ctx.home.public_key
      )

    announce(socket, record)

    route = %{
      packet: packet(citizen, provider.public_key),
      path: [ctx.right.public_key, ctx.left.public_key, ctx.home.public_key],
      cursor: 2,
      mode: :request,
      record: record
    }

    Relay.federation_routed_packet(ctx.relay, ctx.left.public_key, route)
    assert recv(socket) == route.packet
    assert map_size(:sys.get_state(ctx.relay).federation_network_returns) == 1

    announce(
      socket,
      RelayAnnouncement.create(provider, [],
        federation: :direct,
        relay_public_key: ctx.home.public_key
      )
    )

    Relay.federation_routed_packet(ctx.relay, ctx.left.public_key, route)
    assert {:error, :timeout} = :gen_tcp.recv(socket, 4, 100)
  end

  test "an ordinary citizen receives only bound replies on its original local connection", ctx do
    provider = Identity.generate()
    citizen = Identity.generate()
    far_home = Identity.generate().public_key
    socket = connect(ctx, citizen)
    announce(socket, RelayAnnouncement.create(citizen, []))
    conn = Relay.route_for(ctx.relay, citizen.public_key)

    record =
      RelayAnnouncement.create(provider, [], federation: :network, relay_public_key: far_home)

    {:ok, entry} = RelayAnnouncement.verify(record)
    entry = Map.put(entry, :relay_path, [ctx.left.public_key, far_home])

    :sys.replace_state(
      ctx.relay,
      &%{
        &1
        | federation_entries: %{
            provider.public_key => %{
              peer: ctx.left.public_key,
              entry: entry,
              expires_at: entry.expires_at
            }
          }
      }
    )

    sid = :crypto.strong_rand_bytes(16)
    request = packet(citizen, provider.public_key, sid)
    Relay.route_packet(ctx.relay, conn, request)
    assert_receive {:forwarded, _, sent}
    assert sent.packet == request

    reply = %{
      sent
      | path: Enum.reverse(sent.path),
        cursor: 2,
        mode: :reply,
        record: nil,
        packet: packet(provider, citizen.public_key, sid)
    }

    Relay.federation_routed_packet(ctx.relay, ctx.right.public_key, reply)

    Relay.federation_routed_packet(ctx.relay, ctx.left.public_key, %{
      reply
      | packet: packet(provider, citizen.public_key)
    })

    assert {:error, :timeout} = :gen_tcp.recv(socket, 4, 100)
    Relay.federation_routed_packet(ctx.relay, ctx.left.public_key, reply)
    assert recv(socket) == reply.packet

    replacement = connect(ctx, citizen)
    announce(replacement, RelayAnnouncement.create(citizen, []))
    Relay.federation_routed_packet(ctx.relay, ctx.left.public_key, reply)
    assert {:error, :timeout} = :gen_tcp.recv(replacement, 4, 100)
    assert :sys.get_state(ctx.relay).federation_network_returns == %{}
  end

  test "a departed branch cannot erase healthy search results or reinstall its stale routes",
       ctx do
    citizen = Identity.generate()
    socket = connect(ctx, citizen)
    announce(socket, RelayAnnouncement.create(citizen, []))
    control = %{"type" => "search", "query" => "files", "request_id" => String.duplicate("a", 32)}
    body = "ARC_DIRECTORY_V1" <> IO.iodata_to_binary(:json.encode(control))
    :ok = :gen_tcp.send(socket, <<byte_size(body)::32-big, body::binary>>)
    assert_receive {:directory_query, first, _, first_from}
    assert_receive {:directory_query, second, _, second_from}
    callers = %{first => first_from, second => second_from}

    healthy =
      RelayAnnouncement.create(Identity.generate(), [%{"id" => "files"}],
        federation: :network,
        relay_public_key: ctx.left.public_key
      )

    stale =
      RelayAnnouncement.create(Identity.generate(), [%{"id" => "files"}],
        federation: :network,
        relay_public_key: ctx.right.public_key
      )

    send(ctx.relay, {:federation_peer_down, ctx.right.public_key})
    assert map_size(:sys.get_state(ctx.relay).federation_queries) == 1
    GenServer.reply(callers[ctx.right.public_key], {:ok, %{"entries" => [stale]}})
    GenServer.reply(callers[ctx.left.public_key], {:ok, %{"entries" => [healthy]}})
    <<"ARC_DIRECTORY_V1", payload::binary>> = recv(socket)
    assert %{"ok" => true, "partial" => true, "entries" => [^healthy]} = :json.decode(payload)
    stale_key = Base.decode16!(stale["public_key"], case: :lower)
    refute Map.has_key?(:sys.get_state(ctx.relay).federation_entries, stale_key)
  end

  test "an exact public key remains usable in partial results while prefixes fail closed", ctx do
    citizen = Identity.generate()
    socket = connect(ctx, citizen)
    announce(socket, RelayAnnouncement.create(citizen, []))

    record =
      RelayAnnouncement.create(Identity.generate(), [%{"id" => "files"}],
        federation: :network,
        relay_public_key: ctx.left.public_key
      )

    for query <- [record["public_key"], binary_part(record["public_key"], 0, 8)] do
      control = %{
        "type" => "resolve",
        "query" => query,
        "request_id" => Base.encode16(:crypto.strong_rand_bytes(16), case: :lower)
      }

      body = "ARC_DIRECTORY_V1" <> IO.iodata_to_binary(:json.encode(control))
      :ok = :gen_tcp.send(socket, <<byte_size(body)::32-big, body::binary>>)
      assert_receive {:directory_query, first, _, first_from}
      assert_receive {:directory_query, second, _, second_from}
      callers = %{first => first_from, second => second_from}
      GenServer.reply(callers[ctx.right.public_key], {:error, :federation_timeout})
      GenServer.reply(callers[ctx.left.public_key], {:ok, %{"entries" => [record]}})
      <<"ARC_DIRECTORY_V1", payload::binary>> = recv(socket)

      if query == record["public_key"] do
        assert %{"ok" => true, "partial" => true, "entries" => [^record]} = :json.decode(payload)
      else
        assert %{"ok" => false, "error" => "federation_unavailable"} = :json.decode(payload)
      end
    end
  end

  test "citizen connect survives a network lookup longer than the old five-second deadline",
       ctx do
    :ok = Arc.Net.TransportManager.reset()
    citizen = Identity.generate()
    provider = Identity.generate()
    {:ok, agent} = Arc.Data.Agent.start_link(citizen)

    on_exit(fn ->
      Arc.Net.TransportManager.reset()
      if Process.alive?(agent), do: GenServer.stop(agent, :normal)
    end)

    assert :ok = Arc.Net.connect_relay(~c"127.0.0.1", ctx.port, citizen, ctx.home.public_key)
    assert :ok = Arc.Data.Agent.publish_relay(agent)

    task =
      Task.async(fn -> Arc.Data.Agent.connect(agent, Identity.encode_public_key(provider)) end)

    assert_receive {:directory_query, first, _, first_from}
    assert_receive {:directory_query, second, _, second_from}
    callers = %{first => first_from, second => second_from}
    # Real transport and citizen call stay pending while both partner branches
    # are delayed. This is deliberately beyond GenServer.call's default timeout.
    receive do
    after
      5_500 -> :ok
    end

    record =
      RelayAnnouncement.create(provider, [],
        federation: :network,
        relay_public_key: ctx.left.public_key
      )

    GenServer.reply(callers[ctx.left.public_key], {:ok, %{"entries" => [record]}})
    GenServer.reply(callers[ctx.right.public_key], {:ok, %{"entries" => []}})
    assert {:ok, entry} = Task.await(task, 2_000)
    assert entry.public_key == provider.public_key
  end

  test "recursive queries stop when their requesting peer leaves", ctx do
    :sys.replace_state(ctx.relay, &%{&1 | federation_transit: true})

    request =
      Arc.Net.Relay.FederationDirectory.originate(
        %{"type" => "search", "query" => "files"},
        ctx.left.public_key
      )

    caller =
      Task.async(fn -> Relay.federation_request(ctx.relay, ctx.left.public_key, request) end)

    assert_receive {:directory_query, _, _, _}
    state = :sys.get_state(ctx.relay)
    [query] = Map.values(state.federation_queries)
    ref = Process.monitor(query.pid)
    send(ctx.relay, {:federation_peer_down, ctx.left.public_key})
    assert %{"error" => "federation_unavailable"} = Task.await(caller)
    assert_receive {:DOWN, ^ref, :process, _, :shutdown}
    assert :sys.get_state(ctx.relay).federation_queries == %{}
  end

  test "expired transit permissions cannot carry replies", ctx do
    :sys.replace_state(ctx.relay, &%{&1 | federation_transit: true})
    provider = Identity.generate()
    citizen = Identity.generate()
    sid = :crypto.strong_rand_bytes(16)
    route = transit_request(ctx, provider, citizen, sid)
    Relay.federation_routed_packet(ctx.relay, ctx.left.public_key, route)
    assert_receive {:forwarded, _, _}

    :sys.replace_state(ctx.relay, fn state ->
      returns =
        Map.new(state.federation_network_returns, fn {key, value} ->
          {key, %{value | expires_at: 0}}
        end)

      %{state | federation_network_returns: returns}
    end)

    reply = %{
      route
      | path: Enum.reverse(route.path),
        mode: :reply,
        record: nil,
        packet: packet(provider, citizen.public_key, sid)
    }

    Relay.federation_routed_packet(ctx.relay, ctx.right.public_key, reply)
    assert :sys.get_state(ctx.relay).federation_network_returns == %{}
    refute_receive {:forwarded, _, _}, 50
  end

  defp transit_request(
         ctx,
         provider \\ Identity.generate(),
         citizen \\ Identity.generate(),
         sid \\ :crypto.strong_rand_bytes(16)
       ) do
    %{
      packet: packet(citizen, provider.public_key, sid),
      path: [ctx.left.public_key, ctx.home.public_key, ctx.right.public_key],
      cursor: 1,
      mode: :request,
      record:
        RelayAnnouncement.create(provider, [%{"id" => "files"}],
          federation: :network,
          relay_public_key: ctx.right.public_key
        )
    }
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

  defp announce(socket, record) do
    control = %{
      "type" => "announce",
      "request_id" => Base.encode16(:crypto.strong_rand_bytes(16), case: :lower),
      "record" => record
    }

    body = "ARC_DIRECTORY_V1" <> IO.iodata_to_binary(:json.encode(control))
    :ok = :gen_tcp.send(socket, <<byte_size(body)::32-big, body::binary>>)
    <<"ARC_DIRECTORY_V1", payload::binary>> = recv(socket)
    assert %{"ok" => true} = :json.decode(payload)
  end

  defp recv(socket) do
    {:ok, <<size::32-big>>} = :gen_tcp.recv(socket, 4, 1_000)
    {:ok, body} = :gen_tcp.recv(socket, size, 1_000)
    body
  end

  defp packet(from, to, sid \\ :crypto.strong_rand_bytes(16)),
    do:
      Packet.encode(
        from,
        to,
        sid,
        0,
        :crypto.strong_rand_bytes(12),
        :crypto.strong_rand_bytes(32)
      )
end
