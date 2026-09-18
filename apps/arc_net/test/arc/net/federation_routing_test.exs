defmodule Arc.Net.FederationRoutingTest do
  use ExUnit.Case, async: false

  alias Arc.Data.{Packet, RelayAnnouncement}
  alias Arc.Identity
  alias Arc.Net.{Handshake, Relay}

  setup do
    # Keep this relay on the inbound-only side of the deterministic peer link.
    [peer, home] = Enum.sort_by([Identity.generate(), Identity.generate()], & &1.public_key)

    {:ok, relay} =
      Relay.start_link(0,
        relay_identity: home,
        federation_peers: [%{public_key: peer.public_key, host: ~c"127.0.0.1", port: 1}]
      )

    on_exit(fn -> Arc.Net.TestTeardown.stop(relay) end)
    %{relay: relay, home: home, peer: peer, port: Relay.get_port(relay)}
  end

  test "peer delivery requires a locally shared destination and creates a bounded return path",
       ctx do
    provider = Identity.generate()
    citizen = Identity.generate()
    socket = connect(ctx, provider)
    announce(socket, RelayAnnouncement.create(provider, [%{"id" => "files"}]))
    packet = packet(citizen, provider.public_key)

    # These calls model packets after federation transport authentication.
    Relay.federation_packet(ctx.relay, ctx.peer.public_key, packet)
    assert {:error, :timeout} = :gen_tcp.recv(socket, 4, 100)

    announce(
      socket,
      RelayAnnouncement.create(provider, [%{"id" => "files"}],
        federation: :direct,
        relay_public_key: ctx.home.public_key
      )
    )

    Relay.federation_packet(ctx.relay, ctx.peer.public_key, packet)
    assert recv(socket) == packet

    {:ok, decoded} = Packet.decode(packet)
    state = :sys.get_state(ctx.relay)

    assert %{kind: :inbound, peer: peer} =
             state.federation_returns[
               {provider.public_key, citizen.public_key, decoded.session_id}
             ]

    assert peer == ctx.peer.public_key
    refute Map.has_key?(state.directory_records, citizen.public_key)

    send(ctx.relay, {:federation_peer_down, ctx.peer.public_key})
    assert :sys.get_state(ctx.relay).federation_returns == %{}
  end

  test "unapproved peers, private recipients, and onward transit are rejected", ctx do
    private = Identity.generate()
    sender = Identity.generate()
    socket = connect(ctx, private)
    announce(socket, RelayAnnouncement.create(private, []))
    Relay.federation_packet(ctx.relay, ctx.peer.public_key, packet(sender, private.public_key))
    assert {:error, :timeout} = :gen_tcp.recv(socket, 4, 100)

    announce(
      socket,
      RelayAnnouncement.create(private, [],
        federation: :direct,
        relay_public_key: ctx.home.public_key
      )
    )

    Relay.federation_packet(
      ctx.relay,
      Identity.generate().public_key,
      packet(sender, private.public_key)
    )

    assert {:error, :timeout} = :gen_tcp.recv(socket, 4, 100)

    Relay.federation_packet(
      ctx.relay,
      ctx.peer.public_key,
      packet(sender, Identity.generate().public_key)
    )

    state = :sys.get_state(ctx.relay)
    assert state.federation_queries == %{}
    assert state.federation_returns == %{}
  end

  test "a publisher cannot authorize export from a different relay", ctx do
    provider = Identity.generate()
    socket = connect(ctx, provider)

    record =
      RelayAnnouncement.create(provider, [],
        federation: :direct,
        relay_public_key: ctx.peer.public_key
      )

    send_control(socket, record)
    <<"ARC_DIRECTORY_V1", payload::binary>> = recv(socket)
    assert %{"ok" => false, "error" => "invalid_announcement"} = :json.decode(payload)
  end

  test "an authenticated citizen cannot negotiate federation without operator approval", ctx do
    socket = connect(ctx, Identity.generate())
    body = "ARC_FEDERATION_V1{}"
    :ok = :gen_tcp.send(socket, <<byte_size(body)::32-big, body::binary>>)
    assert {:error, :closed} = :gen_tcp.recv(socket, 4, 1_000)
    assert Process.alive?(ctx.relay)
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
    send_control(socket, record)
    <<"ARC_DIRECTORY_V1", payload::binary>> = recv(socket)
    assert %{"ok" => true} = :json.decode(payload)
  end

  defp send_control(socket, record) do
    control = %{
      "type" => "announce",
      "request_id" => Base.encode16(:crypto.strong_rand_bytes(16), case: :lower),
      "record" => record
    }

    body = "ARC_DIRECTORY_V1" <> IO.iodata_to_binary(:json.encode(control))
    :ok = :gen_tcp.send(socket, <<byte_size(body)::32-big, body::binary>>)
  end

  defp recv(socket) do
    {:ok, <<size::32-big>>} = :gen_tcp.recv(socket, 4, 1_000)
    {:ok, body} = :gen_tcp.recv(socket, size, 1_000)
    body
  end

  defp packet(from, to) do
    Packet.encode(
      from,
      to,
      :crypto.strong_rand_bytes(16),
      0,
      :crypto.strong_rand_bytes(12),
      :crypto.strong_rand_bytes(32)
    )
  end
end
