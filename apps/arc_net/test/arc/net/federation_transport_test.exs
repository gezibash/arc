defmodule Arc.Net.FederationTransportTest do
  use ExUnit.Case, async: false

  alias Arc.Data.RelayAnnouncement
  alias Arc.Identity
  alias Arc.Net.{Federation, Handshake}

  @prefix "ARC_FEDERATION_V1"
  @proof_prefix "ARC_FEDERATION_PROOF_V1"

  test "accepts a fresh, signed federation proof before marking a peer connected" do
    manager = Identity.generate()
    peer = greater_identity(manager.public_key)
    {port, server} = start_peer(peer, :valid)
    relay = relay_process()

    {:ok, federation} =
      Federation.start_link(
        relay: relay,
        identity: manager,
        peers: [%{public_key: peer.public_key, host: ~c"localhost", port: port}]
      )

    assert wait_until(fn -> peer.public_key in Federation.connected_peers(federation) end)

    stop_federation(federation)
    stop_relay(relay)
    Task.await(server, 2_000)
  end

  test "notifies its relay once after an authenticated federation link is ready" do
    manager = Identity.generate()
    peer = greater_identity(manager.public_key)
    {port, server} = start_peer(peer, :valid)
    relay = relay_process(self())

    {:ok, federation} =
      Federation.start_link(
        relay: relay,
        identity: manager,
        peers: [%{public_key: peer.public_key, host: ~c"localhost", port: port}]
      )

    assert wait_until(fn -> peer.public_key in Federation.connected_peers(federation) end)
    assert_receive {:federation_peer_up, peer_key}
    assert peer_key == peer.public_key
    refute_receive {:federation_peer_up, ^peer_key}, 100

    stop_federation(federation)
    stop_relay(relay)
    Task.await(server, 2_000)
  end

  test "rejects a server proof that is not signed by the configured relay identity" do
    manager = Identity.generate()
    peer = greater_identity(manager.public_key)
    {port, server} = start_peer(peer, :wrong_proof)
    relay = relay_process()

    {:ok, federation} =
      Federation.start_link(
        relay: relay,
        identity: manager,
        peers: [%{public_key: peer.public_key, host: ~c"localhost", port: port}]
      )

    refute wait_until(fn -> peer.public_key in Federation.connected_peers(federation) end, 8)

    stop_federation(federation)
    stop_relay(relay)
    Task.await(server, 2_000)
  end

  test "times out one unanswered request and remains usable" do
    manager = Identity.generate()
    peer = greater_identity(manager.public_key)
    {port, server} = start_peer(peer, :ignore_requests)
    relay = relay_process()

    {:ok, federation} =
      Federation.start_link(
        relay: relay,
        identity: manager,
        peers: [%{public_key: peer.public_key, host: ~c"localhost", port: port}]
      )

    assert wait_until(fn -> peer.public_key in Federation.connected_peers(federation) end)

    assert {:error, :federation_timeout} =
             Federation.request(federation, peer.public_key, %{
               "type" => "search",
               "query" => "files",
               "limit" => 1
             })

    assert peer.public_key in Federation.connected_peers(federation)

    stop_federation(federation)
    stop_relay(relay)
    Task.await(server, 2_000)
  end

  test "carries a forwarded packet larger than the request control limit" do
    manager = Identity.generate()
    peer = greater_identity(manager.public_key)
    {port, server} = start_peer(peer, :observe_forward)
    relay = relay_process()

    {:ok, federation} =
      Federation.start_link(
        relay: relay,
        identity: manager,
        peers: [%{public_key: peer.public_key, host: ~c"localhost", port: port}]
      )

    assert wait_until(fn -> peer.public_key in Federation.connected_peers(federation) end)

    assert :ok =
             Federation.forward(
               federation,
               peer.public_key,
               :crypto.strong_rand_bytes(300 * 1024)
             )

    stop_federation(federation)
    stop_relay(relay)
    assert :ok = Task.await(server, 2_000)
  end

  test "drops a connected peer that replays an unencrypted handshake frame" do
    manager = Identity.generate()
    peer = greater_identity(manager.public_key)
    {port, server} = start_peer(peer, :replay_proof)
    relay = relay_process()

    {:ok, federation} =
      Federation.start_link(
        relay: relay,
        identity: manager,
        peers: [%{public_key: peer.public_key, host: ~c"localhost", port: port}]
      )

    Process.sleep(200)
    refute peer.public_key in Federation.connected_peers(federation)

    stop_federation(federation)
    stop_relay(relay)
    Task.await(server, 2_000)
  end

  test "sends a routed packet over the encrypted federation channel" do
    manager = Identity.generate()
    peer = greater_identity(manager.public_key)
    {port, server} = start_peer(peer, :observe_route)
    relay = relay_process()

    {:ok, federation} =
      Federation.start_link(
        relay: relay,
        identity: manager,
        peers: [%{public_key: peer.public_key, host: ~c"localhost", port: port}]
      )

    assert wait_until(fn -> peer.public_key in Federation.connected_peers(federation) end)

    route = %{
      packet: <<1, 2, 3>>,
      path: [manager.public_key, peer.public_key],
      cursor: 1,
      mode: :reply,
      record: nil
    }

    assert :ok = Federation.forward_route(federation, peer.public_key, route)
    stop_federation(federation)
    stop_relay(relay)
    assert :ok = Task.await(server, 2_000)
  end

  defp start_peer(peer, mode) do
    {:ok, listen} = :gen_tcp.listen(0, [:binary, packet: :raw, active: false, reuseaddr: true])
    {:ok, {_address, port}} = :inet.sockname(listen)

    task =
      Task.async(fn ->
        {:ok, socket} = :gen_tcp.accept(listen, 2_000)
        relay_key = :crypto.strong_rand_bytes(32)
        challenge = :crypto.strong_rand_bytes(32)
        :ok = :gen_tcp.send(socket, Handshake.relay_hello(relay_key, challenge) |> elem(1))
        :ok = send_frame(socket, Handshake.relay_info(:unbounded))
        {:ok, hello} = :gen_tcp.recv(socket, 96, 2_000)
        <<client_pk::binary-size(32), signature::binary-size(64)>> = hello
        assert Handshake.verify_client_hello(client_pk, signature, relay_key, challenge)

        {:ok, federation_hello} = recv_frame(socket)
        <<@prefix, payload::binary>> = federation_hello

        %{"type" => "hello", "nonce" => nonce_hex, "announcement" => announcement} =
          :json.decode(payload)

        {:ok, client_nonce} = Base.decode16(nonce_hex, case: :mixed)
        assert {:ok, entry} = RelayAnnouncement.verify(announcement)
        assert entry.public_key == client_pk

        server_nonce = :crypto.strong_rand_bytes(32)
        peer_announcement = RelayAnnouncement.create(peer, [])
        {peer_x, _} = Identity.to_x25519(peer)

        proof =
          case mode do
            :wrong_proof ->
              :crypto.strong_rand_bytes(64)

            _ ->
              Identity.sign(
                peer,
                proof_message(client_pk, peer.public_key, client_nonce, server_nonce, peer_x)
              )
          end

        response = %{
          "type" => "proof",
          "nonce" => Base.encode16(server_nonce, case: :lower),
          "announcement" => peer_announcement,
          "proof" => Base.encode16(proof, case: :lower)
        }

        :ok = send_frame(socket, @prefix <> (response |> :json.encode() |> IO.iodata_to_binary()))

        replay_proof(socket, response, mode)
        handle_peer_mode(socket, mode)

        :gen_tcp.close(socket)
        :gen_tcp.close(listen)
      end)

    {port, task}
  end

  defp replay_proof(socket, response, :replay_proof),
    do: send_frame(socket, encoded_proof(response))

  defp replay_proof(_socket, _response, _mode), do: :ok

  defp handle_peer_mode(socket, :ignore_requests) do
    {:ok, _request} = recv_frame(socket)
    Process.sleep(1_400)
  end

  defp handle_peer_mode(socket, mode) when mode in [:observe_forward, :observe_route] do
    {:ok, first} = recv_frame(socket)
    {:ok, second} = recv_frame(socket)
    assert_observed_frames([first, second], mode)
  end

  defp handle_peer_mode(_socket, _mode), do: Process.sleep(500)

  defp assert_observed_frames(frames, :observe_forward),
    do: assert(Enum.any?(frames, &(byte_size(&1) > 300 * 1024)))

  defp assert_observed_frames(frames, :observe_route),
    do: assert(Enum.all?(frames, &(byte_size(&1) > 100)))

  defp encoded_proof(response),
    do: @prefix <> (response |> :json.encode() |> IO.iodata_to_binary())

  defp proof_message(client_pk, server_pk, client_nonce, server_nonce, server_x) do
    @proof_prefix <> client_pk <> server_pk <> client_nonce <> server_nonce <> server_x
  end

  defp send_frame(socket, body),
    do: :gen_tcp.send(socket, <<byte_size(body)::32-big, body::binary>>)

  defp recv_frame(socket) do
    :gen_tcp.recv(socket, 4, 2_000)
    |> recv_frame_body(socket)
  end

  defp recv_frame_body({:ok, <<length::32-big>>}, socket),
    do: :gen_tcp.recv(socket, length, 2_000)

  defp recv_frame_body(error, _socket), do: error

  defp greater_identity(manager_key) do
    candidate = Identity.generate()
    if candidate.public_key > manager_key, do: candidate, else: greater_identity(manager_key)
  end

  defp relay_process(notify \\ nil) do
    spawn_link(fn ->
      receive do
        :stop ->
          :ok

        {:federation_peer_up, peer} ->
          if is_pid(notify), do: send(notify, {:federation_peer_up, peer})
          relay_loop(notify)
      end
    end)
  end

  defp relay_loop(notify) do
    receive do
      :stop ->
        :ok

      {:federation_peer_up, peer} ->
        if is_pid(notify), do: send(notify, {:federation_peer_up, peer})
        relay_loop(notify)
    end
  end

  defp stop_relay(pid) do
    if Process.alive?(pid), do: send(pid, :stop)
  end

  defp stop_federation(pid) do
    Arc.Net.TestTeardown.stop(pid)
  catch
    :exit, _ -> :ok
  end

  defp wait_until(fun, attempts \\ 30)
  defp wait_until(_fun, 0), do: false

  defp wait_until(fun, attempts),
    do:
      if(fun.(),
        do: true,
        else:
          (
            Process.sleep(50)
            wait_until(fun, attempts - 1)
          )
      )
end
