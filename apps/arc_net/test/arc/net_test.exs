defmodule Arc.NetTest do
  use ExUnit.Case, async: false

  alias Arc.Data.Packet
  alias Arc.Identity
  alias Arc.Net.Handshake

  # Build a valid signed Arc packet (ciphertext is dummy — relay never decrypts).
  defp make_packet(src_id, dst_pk) do
    session_id = :crypto.strong_rand_bytes(16)
    nonce = :crypto.strong_rand_bytes(12)
    ciphertext = :crypto.strong_rand_bytes(32)
    Packet.encode(src_id, dst_pk, session_id, 0, nonce, ciphertext)
  end

  defp frame(packet), do: <<byte_size(packet)::32-big, packet::binary>>

  defp recv_framed(sock) do
    {:ok, <<len::32-big>>} = :gen_tcp.recv(sock, 4, 2000)
    {:ok, body} = :gen_tcp.recv(sock, len, 2000)
    <<len::32-big, body::binary>>
  end

  defp relay_connect(port, identity, sock_opts \\ []) do
    {:ok, sock} =
      :gen_tcp.connect(
        ~c"localhost",
        port,
        [:binary, packet: :raw, active: false] ++ sock_opts
      )

    {:ok, relay_hello} = :gen_tcp.recv(sock, 64, 2000)
    {:ok, relay_pubkey, relay_challenge} = Handshake.decode_relay_hello(relay_hello)
    {:ok, _cap} = recv_relay_info(sock)

    {:ok, client_hello, _client_pubkey} =
      Handshake.client_hello(identity, relay_pubkey, relay_challenge)

    :ok = :gen_tcp.send(sock, client_hello)
    sock
  end

  defp recv_relay_info(sock) do
    <<_len::32-big, body::binary>> = recv_framed(sock)
    Handshake.decode_relay_info(body)
  end

  defp put_env(key, value) do
    original = Application.get_env(:arc_net, key)
    Application.put_env(:arc_net, key, value)

    on_exit(fn ->
      if original == nil do
        Application.delete_env(:arc_net, key)
      else
        Application.put_env(:arc_net, key, original)
      end
    end)
  end

  defp clear_transport_connection do
    :ok = Arc.Net.TransportManager.reset()
    Process.sleep(80)
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

  defp wait_until(fun, attempts \\ 20)

  defp wait_until(fun, attempts) when attempts > 0 do
    if fun.() do
      true
    else
      Process.sleep(50)
      wait_until(fun, attempts - 1)
    end
  end

  defp wait_until(_fun, 0), do: false

  describe "Arc.Net.Relay" do
    setup do
      Application.ensure_all_started(:arc_net)

      {:ok, relay} = Arc.Net.Relay.start_link(0)
      port = Arc.Net.Relay.get_port(relay)

      on_exit(fn ->
        stop_relay(relay)
      end)

      %{relay: relay, port: port}
    end

    test "routes a packet from client A to client B", %{port: port} do
      alice = Identity.generate()
      bob = Identity.generate()

      sock_a = relay_connect(port, alice)
      sock_b = relay_connect(port, bob)

      # Give relay time to register both clients
      Process.sleep(100)

      # Client A sends a packet destined for Bob
      packet = make_packet(alice, bob.public_key)
      :ok = :gen_tcp.send(sock_a, frame(packet))

      # Client B should receive the same framed packet
      received = recv_framed(sock_b)
      assert received == frame(packet)

      :gen_tcp.close(sock_a)
      :gen_tcp.close(sock_b)
    end

    test "silently drops packet when destination is not connected", %{port: port} do
      alice = Identity.generate()
      bob = Identity.generate()

      sock_a = relay_connect(port, alice)
      Process.sleep(50)

      packet = make_packet(alice, bob.public_key)
      assert :ok = :gen_tcp.send(sock_a, frame(packet))

      :gen_tcp.close(sock_a)
    end

    test "cleans up routes when client disconnects", %{relay: relay, port: port} do
      alice = Identity.generate()

      sock_a = relay_connect(port, alice)
      Process.sleep(50)

      # Route should be registered
      assert is_pid(Arc.Net.Relay.route_for(relay, alice.public_key))

      :gen_tcp.close(sock_a)
      Process.sleep(100)

      # Route should be removed after disconnect
      assert Arc.Net.Relay.route_for(relay, alice.public_key) == nil
    end

    test "does not crash when sending to a disconnected client", %{port: port} do
      alice = Identity.generate()
      bob = Identity.generate()

      sock_a = relay_connect(port, alice)
      sock_b = relay_connect(port, bob)
      Process.sleep(50)

      # Disconnect Bob
      :gen_tcp.close(sock_b)
      Process.sleep(50)

      # Alice sends to Bob — relay should drop silently without crashing
      packet = make_packet(alice, bob.public_key)
      assert :ok = :gen_tcp.send(sock_a, frame(packet))
      Process.sleep(50)

      :gen_tcp.close(sock_a)
    end

    test "keeps route for newest connection when same pubkey reconnects", %{port: port} do
      alice = Identity.generate()
      bob = Identity.generate()

      sock_a = relay_connect(port, alice)
      sock_b_old = relay_connect(port, bob)
      Process.sleep(60)
      sock_b_new = relay_connect(port, bob)
      Process.sleep(120)

      :gen_tcp.close(sock_b_old)
      Process.sleep(120)

      packet = make_packet(alice, bob.public_key)
      :ok = :gen_tcp.send(sock_a, frame(packet))
      received = recv_framed(sock_b_new)
      assert received == frame(packet)

      :gen_tcp.close(sock_a)
      :gen_tcp.close(sock_b_new)
    end

    test "drops packet when signed src does not match hello pubkey", %{port: port} do
      hello_id = Identity.generate()
      forged_src = Identity.generate()
      bob = Identity.generate()

      sock_bad = relay_connect(port, hello_id)
      sock_b = relay_connect(port, bob)
      Process.sleep(100)

      forged_packet = make_packet(forged_src, bob.public_key)
      :ok = :gen_tcp.send(sock_bad, frame(forged_packet))

      assert {:error, :timeout} = :gen_tcp.recv(sock_b, 4, 200)

      :gen_tcp.close(sock_bad)
      :gen_tcp.close(sock_b)
    end

    test "closes connection when frame length exceeds max_frame_bytes", %{port: port} do
      put_env(:max_frame_bytes, 128)

      alice = Identity.generate()

      sock_a = relay_connect(port, alice)
      Process.sleep(60)

      # Header advertises a frame larger than max_frame_bytes.
      :ok = :gen_tcp.send(sock_a, <<1024::32-big>>)
      assert {:error, :closed} = :gen_tcp.recv(sock_a, 1, 500)
    end

    test "advertises the frame cap in the relay info frame", %{port: port} do
      put_env(:max_frame_bytes, 4096)

      {:ok, sock} = :gen_tcp.connect(~c"localhost", port, [:binary, packet: :raw, active: false])
      {:ok, _relay_hello} = :gen_tcp.recv(sock, 64, 2000)
      assert recv_relay_info(sock) == {:ok, 4096}
      :gen_tcp.close(sock)
    end

    test "advertises unbounded when no cap is set", %{port: port} do
      Application.delete_env(:arc_net, :max_frame_bytes)

      {:ok, sock} = :gen_tcp.connect(~c"localhost", port, [:binary, packet: :raw, active: false])
      {:ok, _relay_hello} = :gen_tcp.recv(sock, 64, 2000)
      assert recv_relay_info(sock) == {:ok, :unbounded}
      :gen_tcp.close(sock)
    end

    test "client refuses a send over the advertised cap without dropping the connection", %{
      port: port
    } do
      put_env(:max_frame_bytes, 256)

      alice = Identity.generate()
      bob = Identity.generate()

      {:ok, sock} = :gen_tcp.connect(~c"localhost", port, [:binary, packet: :raw, active: false])
      {:ok, relay_hello} = :gen_tcp.recv(sock, 64, 2000)
      {:ok, relay_pubkey, relay_challenge} = Handshake.decode_relay_hello(relay_hello)
      {:ok, client_hello, _} = Handshake.client_hello(alice, relay_pubkey, relay_challenge)
      :ok = :gen_tcp.send(sock, client_hello)

      {:ok, conn} =
        Arc.Net.Connection.start_link(socket: sock, role: :client, transport_pid: self())

      :ok = :gen_tcp.controlling_process(sock, conn)
      Arc.Net.Connection.activate(conn)

      assert wait_until(fn -> Arc.Net.Connection.peer_max_frame_bytes(conn) == 256 end)

      big = make_packet(alice, bob.public_key)
      assert byte_size(big) > 256
      assert Arc.Net.Connection.send_packet(conn, big) == {:error, :frame_too_large}
      assert Process.alive?(conn)

      # The info frame never reaches the transport as a packet.
      refute_received {:packet_received, _}

      Arc.Net.Connection.close(conn)
    end

    test "relays a 20 MiB frame end to end with no cap set", %{port: port} do
      Application.delete_env(:arc_net, :max_frame_bytes)

      alice = Identity.generate()
      bob = Identity.generate()

      sock_a = relay_connect(port, alice)
      sock_b = relay_connect(port, bob, buffer: 1_048_576)
      Process.sleep(100)

      ciphertext = :crypto.strong_rand_bytes(20 * 1024 * 1024)
      session_id = :crypto.strong_rand_bytes(16)
      nonce = :crypto.strong_rand_bytes(12)
      packet = Packet.encode(alice, bob.public_key, session_id, 0, nonce, ciphertext)
      assert byte_size(packet) > 20 * 1024 * 1024

      :ok = :gen_tcp.send(sock_a, frame(packet))

      {:ok, <<len::32-big>>} = :gen_tcp.recv(sock_b, 4, 10_000)
      assert len == byte_size(packet)
      {:ok, body} = :gen_tcp.recv(sock_b, len, 30_000)
      assert body == packet

      :gen_tcp.close(sock_a)
      :gen_tcp.close(sock_b)
    end

    test "closes connection when hello is not completed before timeout", %{port: port} do
      put_env(:hello_timeout_ms, 100)

      {:ok, sock} = :gen_tcp.connect(~c"localhost", port, [:binary, packet: :raw, active: false])

      {:ok, <<_relay_pubkey::binary-size(32), _relay_challenge::binary-size(32)>>} =
        :gen_tcp.recv(sock, 64, 2000)

      {:ok, _cap} = recv_relay_info(sock)

      # Send less than full signed hello and wait for timeout.
      :ok = :gen_tcp.send(sock, <<1, 2, 3, 4>>)

      assert {:error, :closed} = :gen_tcp.recv(sock, 1, 700)
    end

    test "rejects client hello with invalid signature", %{port: port} do
      bad_id = Identity.generate()

      {:ok, sock} = :gen_tcp.connect(~c"localhost", port, [:binary, packet: :raw, active: false])

      {:ok, <<_relay_pubkey::binary-size(32), _relay_challenge::binary-size(32)>>} =
        :gen_tcp.recv(sock, 64, 2000)

      {:ok, _cap} = recv_relay_info(sock)

      bad_hello = <<bad_id.public_key::binary, :crypto.strong_rand_bytes(64)::binary>>
      :ok = :gen_tcp.send(sock, bad_hello)

      assert {:error, :closed} = :gen_tcp.recv(sock, 1, 700)
    end

    test "restarts acceptor when it crashes", %{relay: relay, port: port} do
      before = :sys.get_state(relay)
      [old_acceptor | _] = Map.keys(before.acceptor_refs)
      Process.exit(old_acceptor, :kill)
      Process.sleep(200)

      after_state = :sys.get_state(relay)
      refute Map.has_key?(after_state.acceptor_refs, old_acceptor)
      assert map_size(after_state.acceptor_refs) == before.acceptor_count
      assert Enum.all?(Map.keys(after_state.acceptor_refs), &Process.alive?/1)

      alice = Identity.generate()
      bob = Identity.generate()

      sock_a = relay_connect(port, alice)
      sock_b = relay_connect(port, bob)
      Process.sleep(100)

      packet = make_packet(alice, bob.public_key)
      :ok = :gen_tcp.send(sock_a, frame(packet))
      assert recv_framed(sock_b) == frame(packet)

      :gen_tcp.close(sock_a)
      :gen_tcp.close(sock_b)
    end

    test "restarts route shard when it crashes", %{relay: relay} do
      before = :sys.get_state(relay)
      [{idx, old_shard} | _] = Enum.take(before.shard_pids, 1)
      Process.exit(old_shard, :kill)
      Process.sleep(200)

      after_state = :sys.get_state(relay)
      new_shard = Map.fetch!(after_state.shard_pids, idx)
      refute new_shard == old_shard
      assert Process.alive?(new_shard)
      assert map_size(after_state.shard_pids) == map_size(before.shard_pids)
    end

    test "drops packet when destination connection is over mailbox limit with drop policy", %{
      relay: relay,
      port: port
    } do
      put_env(:connection_max_mailbox_len, 0)
      put_env(:connection_overflow_policy, :drop)

      alice = Identity.generate()
      bob = Identity.generate()

      sock_a = relay_connect(port, alice)
      sock_b = relay_connect(port, bob)
      Process.sleep(100)

      packet = make_packet(alice, bob.public_key)
      :ok = :gen_tcp.send(sock_a, frame(packet))

      assert {:error, :timeout} = :gen_tcp.recv(sock_b, 4, 300)
      route_pid = Arc.Net.Relay.route_for(relay, bob.public_key)
      assert is_pid(route_pid)
      assert Process.alive?(route_pid)

      :gen_tcp.close(sock_a)
      :gen_tcp.close(sock_b)
    end

    test "disconnects destination connection when over mailbox limit with disconnect policy", %{
      relay: relay,
      port: port
    } do
      put_env(:connection_max_mailbox_len, 0)
      put_env(:connection_overflow_policy, :disconnect)

      alice = Identity.generate()
      bob = Identity.generate()

      sock_a = relay_connect(port, alice)
      sock_b = relay_connect(port, bob)
      Process.sleep(100)

      packet = make_packet(alice, bob.public_key)
      :ok = :gen_tcp.send(sock_a, frame(packet))

      assert {:error, :closed} = :gen_tcp.recv(sock_b, 1, 1000)
      assert wait_until(fn -> Arc.Net.Relay.route_for(relay, bob.public_key) == nil end)

      :gen_tcp.close(sock_a)
    end
  end

  describe "Arc.Net.Relay config" do
    test "applies configured acceptor and route partition counts" do
      put_env(:relay_acceptors, 3)
      put_env(:relay_route_partitions, 7)

      {:ok, relay} = Arc.Net.Relay.start_link(0)

      on_exit(fn ->
        stop_relay(relay)
      end)

      state = :sys.get_state(relay)
      assert state.acceptor_count == 3
      assert state.route_partitions == 7
      assert map_size(state.acceptor_refs) == 3
      assert length(state.routes_tables) == 7
    end
  end

  describe "Arc.Net.Relay identity hello" do
    test "sends configured relay pubkey as first bytes on new connection" do
      relay_pubkey = :crypto.strong_rand_bytes(32)
      {:ok, relay} = Arc.Net.Relay.start_link(0, relay_public_key: relay_pubkey)
      port = Arc.Net.Relay.get_port(relay)

      {:ok, sock} = :gen_tcp.connect(~c"localhost", port, [:binary, packet: :raw, active: false])

      assert {:ok, <<^relay_pubkey::binary-size(32), relay_challenge::binary-size(32)>>} =
               :gen_tcp.recv(sock, 64, 2000)

      assert byte_size(relay_challenge) == 32

      :gen_tcp.close(sock)
      stop_relay(relay)
    end
  end

  describe "Arc.Net public API" do
    setup do
      Application.ensure_all_started(:arc_net)
      clear_transport_connection()
      :ok
    end

    test "relay_address/0 returns nil when ARC_RELAY is unset" do
      System.delete_env("ARC_RELAY")
      assert Arc.Net.relay_address() == nil
    end

    test "relay_address/0 parses valid host:port" do
      System.put_env("ARC_RELAY", "myrelay.example.com:7331")
      assert Arc.Net.relay_address() == {~c"myrelay.example.com", 7331}
    after
      System.delete_env("ARC_RELAY")
    end

    test "relay_pubkey/0 returns nil when ARC_RELAY_PUBKEY is unset" do
      System.delete_env("ARC_RELAY_PUBKEY")
      assert Arc.Net.relay_pubkey() == nil
    end

    test "relay_pubkey/0 parses ARC_RELAY_PUBKEY from hex" do
      pubkey = :crypto.strong_rand_bytes(32)
      System.put_env("ARC_RELAY_PUBKEY", Base.encode16(pubkey, case: :lower))
      assert Arc.Net.relay_pubkey() == pubkey
    after
      System.delete_env("ARC_RELAY_PUBKEY")
    end

    test "relay_address_from/1 parses host:port string" do
      assert Arc.Net.relay_address_from("localhost:7331") == {~c"localhost", 7331}
    end

    test "parse_frame_cap/1 accepts a byte count, 0, or unbounded" do
      assert Arc.Net.parse_frame_cap("4194304") == {:ok, 4_194_304}
      assert Arc.Net.parse_frame_cap(" 0 ") == {:ok, :unbounded}
      assert Arc.Net.parse_frame_cap("unbounded") == {:ok, :unbounded}
      assert Arc.Net.parse_frame_cap("4294967295") == {:ok, 4_294_967_295}
    end

    test "parse_frame_cap/1 rejects negative, non-numeric, and over-header values" do
      assert Arc.Net.parse_frame_cap("-1") == :error
      assert Arc.Net.parse_frame_cap("4MiB") == :error
      assert Arc.Net.parse_frame_cap("") == :error
      assert Arc.Net.parse_frame_cap("4294967296") == :error
      assert Arc.Net.parse_frame_cap(nil) == :error
    end

    test "configure_frame_cap/1 prefers the flag over ARC_RELAY_MAX_FRAME_BYTES" do
      put_env(:max_frame_bytes, nil)
      System.put_env("ARC_RELAY_MAX_FRAME_BYTES", "1024")

      assert Arc.Net.configure_frame_cap("2048") == :ok
      assert Application.get_env(:arc_net, :max_frame_bytes) == 2048

      assert Arc.Net.configure_frame_cap(nil) == :ok
      assert Application.get_env(:arc_net, :max_frame_bytes) == 1024

      assert Arc.Net.configure_frame_cap("bogus") == {:error, :invalid}

      System.delete_env("ARC_RELAY_MAX_FRAME_BYTES")
      assert Arc.Net.configure_frame_cap(nil) == :unset
    after
      System.delete_env("ARC_RELAY_MAX_FRAME_BYTES")
    end

    test "relay_address_from/1 parses bracketed IPv6 host" do
      assert Arc.Net.relay_address_from("[::1]:7331") == {~c"::1", 7331}
    end

    test "relay_address_from/1 returns nil for malformed input" do
      assert Arc.Net.relay_address_from("notaport") == nil
      assert Arc.Net.relay_address_from(nil) == nil
    end

    test "relay_pubkey_from/1 parses hex and base64" do
      pubkey = :crypto.strong_rand_bytes(32)
      hex = Base.encode16(pubkey, case: :lower)
      b64 = Base.encode64(pubkey)

      assert Arc.Net.relay_pubkey_from(hex) == pubkey
      assert Arc.Net.relay_pubkey_from("0x" <> hex) == pubkey
      assert Arc.Net.relay_pubkey_from(b64) == pubkey
    end

    test "relay_pubkey_from/1 returns nil for malformed values" do
      assert Arc.Net.relay_pubkey_from("deadbeef") == nil
      assert Arc.Net.relay_pubkey_from(nil) == nil
    end

    test "connect_relay/3 is idempotent for same endpoint and key" do
      {:ok, relay} = Arc.Net.Relay.start_link(0)
      port = Arc.Net.Relay.get_port(relay)
      id = Identity.generate()

      assert :ok = Arc.Net.connect_relay(~c"localhost", port, id)
      assert :ok = Arc.Net.connect_relay(~c"localhost", port, id)
      Process.sleep(120)

      relay_stats = Arc.Net.Relay.stats(relay)
      assert relay_stats.routes == 1
      assert relay_stats.conns == 1

      stop_relay(relay)
    end

    test "connect_relay/3 keeps separate transports for different identities" do
      {:ok, relay} = Arc.Net.Relay.start_link(0)
      port = Arc.Net.Relay.get_port(relay)
      alice = Identity.generate()
      bob = Identity.generate()

      assert :ok = Arc.Net.connect_relay(~c"localhost", port, alice)
      assert :ok = Arc.Net.connect_relay(~c"localhost", port, bob)
      Process.sleep(120)

      relay_stats = Arc.Net.Relay.stats(relay)
      assert relay_stats.routes == 2
      assert relay_stats.conns == 2
      assert Arc.Net.TransportManager.count() == 2

      stop_relay(relay)
    end

    test "leased transports are reaped after the owner exits" do
      {:ok, relay} = Arc.Net.Relay.start_link(0)
      port = Arc.Net.Relay.get_port(relay)
      id = Identity.generate()

      owner =
        spawn(fn ->
          receive do
            :stop -> :ok
          end
        end)

      assert :ok =
               Arc.Net.acquire_relay(~c"localhost", port, id, nil,
                 owner_pid: owner,
                 idle_timeout_ms: 100
               )

      assert wait_until(fn -> Arc.Net.TransportManager.count() == 1 end)
      assert wait_until(fn -> Arc.Net.Relay.stats(relay).conns == 1 end)

      send(owner, :stop)
      assert wait_until(fn -> not Process.alive?(owner) end)
      assert wait_until(fn -> Arc.Net.TransportManager.count() == 0 end)
      assert wait_until(fn -> Arc.Net.Relay.stats(relay).conns == 0 end)

      stop_relay(relay)
    end

    test "connect_relay/4 accepts matching relay pubkey pin" do
      {:ok, relay} = Arc.Net.Relay.start_link(0)
      port = Arc.Net.Relay.get_port(relay)
      relay_pubkey = Arc.Net.Relay.get_pubkey(relay)
      id = Identity.generate()

      assert :ok = Arc.Net.connect_relay(~c"localhost", port, id, relay_pubkey)

      stop_relay(relay)
    end

    test "connect_relay/4 rejects mismatched relay pubkey pin" do
      {:ok, relay} = Arc.Net.Relay.start_link(0)
      port = Arc.Net.Relay.get_port(relay)
      id = Identity.generate()
      wrong_pin = :crypto.strong_rand_bytes(32)

      assert {:error, :relay_pubkey_mismatch} =
               Arc.Net.connect_relay(~c"localhost", port, id, wrong_pin)

      stop_relay(relay)
    end

    test "connect_relay/3 rejects malformed identity" do
      assert {:error, :invalid_identity} = Arc.Net.connect_relay(~c"localhost", 7331, <<1, 2, 3>>)
    end

    test "connect_relay/4 rejects malformed relay pin" do
      assert {:error, :invalid_relay_pubkey_pin} =
               Arc.Net.connect_relay(
                 ~c"localhost",
                 7331,
                 Identity.generate(),
                 <<1, 2, 3>>
               )
    end
  end
end
