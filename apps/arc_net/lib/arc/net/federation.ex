defmodule Arc.Net.Federation do
  @moduledoc """
  Direct, mutually authorised relay federation over the relay TCP listener.

  A federation connection first uses the normal relay hello to authenticate an
  Ed25519 key.  It then exchanges signed, short-lived no-capability
  announcements and a nonce-bound server proof.  After that exchange every
  request, response and forwarded ARC packet is an encrypted `Arc.Data.Packet`.
  The encrypted channel binding changes on every connection, so a frame from a
  previous connection cannot be replayed on a replacement connection.

  This module deliberately has no directory state.  `Arc.Net.Relay` owns
  imported announcements and routes; federation only provides authenticated,
  encrypted transport between explicitly configured peers.
  """

  use GenServer

  alias Arc.Data.{Packet, RelayAnnouncement, Session}
  alias Arc.Identity
  alias Arc.Net.{Connection, Handshake, Relay}
  alias Arc.Net.Relay.FederationRoute

  @prefix "ARC_FEDERATION_PROOF_V1"
  @version 1
  @kind_request 1
  @kind_response 2
  @kind_forward 3
  @kind_ready 4
  @kind_route 5
  @max_peers 16
  @max_pending 32
  @max_inbound_callbacks 32
  @max_inbound_callbacks_per_peer 4
  @max_request_bytes 256 * 1024
  @max_forward_bytes 8 * 1024 * 1024
  @max_route_bytes @max_forward_bytes + 8_192 + 9 * 32 + 10
  @request_timeout 1_250
  @handshake_timeout 1_500
  @connect_timeout 700
  @reconnect_ms 250

  def start_link(opts) do
    GenServer.start_link(__MODULE__, opts)
  end

  def inbound(manager, conn, authenticated_pk, payload)
      when is_pid(manager) and is_pid(conn) and is_binary(authenticated_pk) and is_binary(payload) do
    GenServer.cast(manager, {:inbound, conn, authenticated_pk, payload})
  end

  def connected_peers(manager), do: GenServer.call(manager, :connected_peers)

  def request(manager, peer_pk, request_map)
      when is_binary(peer_pk) and is_map(request_map) do
    request(manager, peer_pk, request_map, @request_timeout)
  end

  def request(manager, peer_pk, request_map, timeout_ms)
      when is_binary(peer_pk) and is_map(request_map) and is_integer(timeout_ms) and
             timeout_ms in 1..11_000 do
    GenServer.call(manager, {:request, peer_pk, request_map, timeout_ms}, timeout_ms + 250)
  catch
    :exit, _ -> {:error, :federation_timeout}
  end

  def request(_, _, _, _), do: {:error, :invalid_federation_request}

  def forward(manager, peer_pk, inner_packet)
      when is_binary(peer_pk) and is_binary(inner_packet) do
    GenServer.call(manager, {:forward, peer_pk, inner_packet}, 1_500)
  catch
    :exit, _ -> {:error, :federation_timeout}
  end

  def forward_route(manager, peer_pk, route) when is_binary(peer_pk) and is_map(route) do
    GenServer.call(manager, {:forward_route, peer_pk, route}, 1_500)
  catch
    :exit, _ -> {:error, :federation_timeout}
  end

  @impl true
  def init(opts) do
    relay = Keyword.fetch!(opts, :relay)
    %Identity{} = identity = Keyword.fetch!(opts, :identity)
    peers = Keyword.fetch!(opts, :peers)

    with {:ok, peers} <- normalize_peers(peers),
         true <- Process.alive?(relay) do
      Process.flag(:trap_exit, true)
      relay_ref = Process.monitor(relay)

      state = %{
        relay: relay,
        relay_ref: relay_ref,
        identity: identity,
        peers: peers,
        links: %{},
        pending: %{},
        callbacks: %{}
      }

      Enum.each(Map.keys(peers), &send(self(), {:connect_peer, &1}))
      {:ok, state}
    else
      false -> {:stop, :relay_not_alive}
      {:error, reason} -> {:stop, reason}
    end
  end

  @impl true
  def handle_cast({:inbound, conn, peer_pk, payload}, state) do
    {:noreply, handle_inbound(state, conn, peer_pk, payload)}
  end

  @impl true
  def handle_call(:connected_peers, _from, state) do
    peers =
      state.links
      |> Enum.filter(fn {_pk, link} -> ready?(link) end)
      |> Enum.map(&elem(&1, 0))

    {:reply, peers, state}
  end

  def handle_call({:request, peer_pk, request_map, timeout_ms}, from, state) do
    with true <- valid_peer_key?(peer_pk),
         true <- Map.has_key?(state.peers, peer_pk),
         true <- map_bytes(request_map) <= @max_request_bytes,
         true <- map_size(state.pending) < @max_pending,
         {:ok, link} <- ready_link(state, peer_pk),
         request_id = :crypto.strong_rand_bytes(16),
         {:ok, state} <-
           send_message(state, peer_pk, link, @kind_request, request_id, request_map) do
      timer = Process.send_after(self(), {:request_timeout, request_id}, timeout_ms)
      pending = Map.put(state.pending, request_id, %{from: from, peer: peer_pk, timer: timer})
      {:noreply, %{state | pending: pending}}
    else
      false -> {:reply, {:error, :invalid_federation_request}, state}
      {:error, reason} -> {:reply, {:error, reason}, state}
    end
  rescue
    _ -> {:reply, {:error, :invalid_federation_request}, state}
  end

  def handle_call({:forward, peer_pk, inner_packet}, _from, state) do
    with true <- valid_peer_key?(peer_pk),
         true <- byte_size(inner_packet) <= @max_forward_bytes,
         {:ok, link} <- ready_link(state, peer_pk),
         {:ok, state} <- send_message(state, peer_pk, link, @kind_forward, nil, inner_packet) do
      {:reply, :ok, state}
    else
      false -> {:reply, {:error, :forward_too_large}, state}
      {:error, reason} -> {:reply, {:error, reason}, state}
    end
  end

  def handle_call({:forward_route, peer_pk, route}, _from, state) do
    with true <- valid_peer_key?(peer_pk),
         {:ok, encoded} <- FederationRoute.encode(route),
         {:ok, link} <- ready_link(state, peer_pk),
         {:ok, state} <- send_message(state, peer_pk, link, @kind_route, nil, encoded) do
      {:reply, :ok, state}
    else
      {:error, reason} -> {:reply, {:error, reason}, state}
      _ -> {:reply, {:error, :invalid_route}, state}
    end
  end

  @impl true
  def handle_info({:connect_peer, peer_pk}, state) do
    state =
      case Map.fetch(state.peers, peer_pk) do
        {:ok, peer} ->
          maybe_dial_peer(state, peer_pk, peer)

        :error ->
          state
      end

    {:noreply, state}
  end

  def handle_info({:federation_frame, conn, payload}, state) do
    state =
      case link_for_conn(state, conn) do
        {peer_pk, link} -> handle_link_frame(state, peer_pk, link, payload)
        nil -> state
      end

    {:noreply, state}
  end

  def handle_info({:dial_connected, peer_pk, attempt, conn, peer_max_frame_bytes}, state) do
    case Map.get(state.links, peer_pk) do
      %{stage: :dialing, attempt: ^attempt, dial_ref: ref} ->
        Process.demonitor(ref, [:flush])
        nonce = :crypto.strong_rand_bytes(32)
        announcement = RelayAnnouncement.create(state.identity, [])
        payload = %{"type" => "hello", "nonce" => hex(nonce), "announcement" => announcement}

        link = %{
          conn: conn,
          conn_ref: Process.monitor(conn),
          role: :outbound,
          stage: :hello_sent,
          client_nonce: nonce,
          announcement: announcement,
          server_nonce: nil,
          channel: nil,
          session: nil,
          received_seq: nil,
          peer_max_frame_bytes: peer_max_frame_bytes
        }

        state =
          put_link(
            state,
            peer_pk,
            Map.put(link, :handshake_timer, handshake_timer(peer_pk, conn))
          )

        case Connection.send_federation(conn, json(payload)) do
          :ok ->
            Connection.activate(conn)
            {:noreply, state}

          _ ->
            Connection.close(conn)
            {:noreply, peer_down(state, peer_pk)}
        end

      _ ->
        Connection.close(conn)
        {:noreply, state}
    end
  end

  def handle_info({:dial_failed, peer_pk, attempt}, state) do
    case Map.get(state.links, peer_pk) do
      %{stage: :dialing, attempt: ^attempt, dial_ref: ref} ->
        Process.demonitor(ref, [:flush])
        links = Map.delete(state.links, peer_pk)
        {:noreply, schedule_connect(%{state | links: links}, peer_pk)}

      _ ->
        {:noreply, state}
    end
  end

  def handle_info({:handshake_timeout, peer_pk, conn}, state) do
    case Map.get(state.links, peer_pk) do
      %{conn: ^conn, stage: stage} when stage in [:hello_sent, :proof_sent] ->
        {:noreply, peer_down(state, peer_pk)}

      _ ->
        {:noreply, state}
    end
  end

  def handle_info({:request_timeout, request_id}, state) do
    {:noreply, complete_pending(state, request_id, {:error, :federation_timeout})}
  end

  def handle_info({:callback_reply, callback_key, peer_pk, request_id, channel, response}, state) do
    case Map.pop(state.callbacks, callback_key) do
      {%{peer: ^peer_pk, channel: ^channel, ref: ref}, callbacks} ->
        Process.demonitor(ref, [:flush])
        state = %{state | callbacks: callbacks}

        case ready_link(state, peer_pk) do
          {:ok, %{channel: ^channel} = link} ->
            state =
              case send_message(state, peer_pk, link, @kind_response, request_id, response) do
                {:ok, state} -> state
                {:error, _} -> state
              end

            {:noreply, state}

          _ ->
            {:noreply, state}
        end

      _ ->
        {:noreply, state}
    end
  end

  def handle_info({:DOWN, ref, :process, _pid, _reason}, %{relay_ref: ref} = state) do
    {:stop, :normal, state}
  end

  def handle_info({:DOWN, ref, :process, conn, _reason}, state) do
    case link_for_conn(state, conn) do
      {peer_pk, %{conn_ref: ^ref}} ->
        {:noreply, peer_down(state, peer_pk)}

      _ ->
        case Enum.find(state.links, fn {_, link} -> Map.get(link, :dial_ref) == ref end) do
          {peer_pk, _} ->
            {:noreply, peer_down(state, peer_pk)}

          nil ->
            callbacks = Map.reject(state.callbacks, fn {_, callback} -> callback.ref == ref end)
            {:noreply, %{state | callbacks: callbacks}}
        end
    end
  end

  def handle_info({:EXIT, conn, _reason}, state) when is_pid(conn) do
    case link_for_conn(state, conn) do
      {peer_pk, _link} -> {:noreply, peer_down(state, peer_pk)}
      _ -> {:noreply, state}
    end
  end

  def handle_info(_message, state), do: {:noreply, state}

  defp maybe_dial_peer(state, peer_pk, peer) do
    if should_dial?(state.identity.public_key, peer_pk) do
      dial_peer_unless_connected(state, peer_pk, peer)
    else
      state
    end
  end

  defp dial_peer_unless_connected(state, peer_pk, peer) do
    case Map.get(state.links, peer_pk) do
      %{stage: :dialing} ->
        state

      %{conn: conn} when is_pid(conn) ->
        if Process.alive?(conn), do: state, else: dial_peer(state, peer_pk, peer)

      _ ->
        dial_peer(state, peer_pk, peer)
    end
  end

  @impl true
  def terminate(_reason, state) do
    Enum.each(state.links, fn {_peer, link} ->
      stop_owned_task(Map.get(link, :dial_pid), Map.get(link, :dial_ref))

      if is_pid(Map.get(link, :conn)) and Process.alive?(link.conn),
        do: Connection.close(link.conn)
    end)

    Enum.each(state.pending, fn {_id, pending} ->
      GenServer.reply(pending.from, {:error, :federation_peer_unavailable})
    end)

    Enum.each(state.callbacks, fn {_, callback} -> stop_owned_task(callback.pid, callback.ref) end)

    :ok
  end

  @impl true
  def format_status(_reason, [_process_dictionary, state]) do
    [data: [{~c"State", redacted_state(state)}]]
  end

  defp dial_peer(state, peer_pk, peer) do
    manager = self()
    identity = state.identity
    attempt = make_ref()

    {:ok, pid} =
      Task.Supervisor.start_child(Arc.Net.TaskSupervisor, fn ->
        result =
          try do
            open_federation_socket(identity, peer, manager)
          rescue
            _ -> {:error, :federation_connect_failed}
          catch
            _, _ -> {:error, :federation_connect_failed}
          end

        case result do
          {:ok, conn, cap} ->
            if Process.alive?(manager),
              do: send(manager, {:dial_connected, peer_pk, attempt, conn, cap}),
              else: Connection.close(conn)

          _ ->
            send(manager, {:dial_failed, peer_pk, attempt})
        end
      end)

    put_link(state, peer_pk, %{
      stage: :dialing,
      attempt: attempt,
      dial_pid: pid,
      dial_ref: Process.monitor(pid)
    })
  end

  defp open_federation_socket(identity, %{host: host, port: port}, manager) do
    with {:ok, host} <- host_charlist(host),
         {:ok, socket} <-
           :gen_tcp.connect(
             host,
             port,
             [:binary, packet: :raw, active: false, keepalive: true],
             @connect_timeout
           ) do
      case open_federation_connection(socket, identity, manager) do
        {:ok, _conn, _cap} = result ->
          result

        {:error, _} = error ->
          :gen_tcp.close(socket)
          error
      end
    end
  end

  defp open_federation_socket(_, _, _), do: {:error, :invalid_peer}

  defp open_federation_connection(socket, identity, manager) do
    with {:ok, relay_pk, challenge, peer_max_frame_bytes} <- recv_hello(socket),
         {:ok, hello, _} <- Handshake.client_hello(identity, relay_pk, challenge),
         :ok <- :gen_tcp.send(socket, hello),
         {:ok, conn} <-
           Connection.start_link(
             socket: socket,
             role: :federation,
             federation_pid: manager,
             peer_max_frame_bytes: peer_max_frame_bytes
           ) do
      case :gen_tcp.controlling_process(socket, conn) do
        :ok ->
          {:ok, conn, peer_max_frame_bytes}

        {:error, _} = error ->
          Connection.close(conn)
          error
      end
    else
      {:error, _} = error ->
        error

      _ ->
        {:error, :federation_connect_failed}
    end
  end

  defp handle_inbound(state, conn, peer_pk, payload) do
    if Map.has_key?(state.peers, peer_pk) and
         byte_size(payload) <= @max_forward_bytes + 512 * 1024 do
      case safe_json(payload) do
        {:ok, %{"type" => "hello", "nonce" => nonce_hex, "announcement" => announcement}} ->
          if byte_size(payload) <= @max_request_bytes,
            do: accept_hello(state, conn, peer_pk, nonce_hex, announcement),
            else: state

        _ ->
          case link_for_conn(state, conn) do
            {^peer_pk, link} -> handle_link_frame(state, peer_pk, link, payload)
            _ -> state
          end
      end
    else
      state
    end
  end

  defp accept_hello(state, conn, peer_pk, nonce_hex, announcement) do
    with {:ok, client_nonce} <- decode_hex(nonce_hex, 32),
         {:ok, entry} <- RelayAnnouncement.verify(announcement),
         true <- entry.public_key == peer_pk do
      server_nonce = :crypto.strong_rand_bytes(32)
      own_announcement = RelayAnnouncement.create(state.identity, [])
      {own_x, _} = Identity.to_x25519(state.identity)

      proof =
        Identity.sign(
          state.identity,
          proof_message(
            peer_pk,
            state.identity.public_key,
            client_nonce,
            server_nonce,
            own_x
          )
        )

      response = %{
        "type" => "proof",
        "nonce" => hex(server_nonce),
        "announcement" => own_announcement,
        "proof" => hex(proof)
      }

      if ready?(Map.get(state.links, peer_pk)) do
        Connection.close(conn)
        state
      else
        case Connection.send_federation(conn, json(response)) do
          :ok ->
            link = %{
              conn: conn,
              conn_ref: Process.monitor(conn),
              role: :inbound,
              stage: :proof_sent,
              client_nonce: client_nonce,
              server_nonce: server_nonce,
              peer_x: entry.x25519_public,
              channel: channel(client_nonce, server_nonce),
              received_seq: nil,
              session: nil
            }

            put_link(
              state,
              peer_pk,
              Map.put(link, :handshake_timer, handshake_timer(peer_pk, conn))
            )

          _ ->
            state
        end
      end
    else
      _ -> state
    end
  rescue
    _ -> state
  end

  defp handle_link_frame(state, peer_pk, %{role: :outbound, stage: :hello_sent} = link, payload) do
    with {:ok,
          %{
            "type" => "proof",
            "nonce" => nonce_hex,
            "announcement" => announcement,
            "proof" => proof_hex
          }} <- safe_json(payload),
         {:ok, server_nonce} <- decode_hex(nonce_hex, 32),
         {:ok, proof} <- decode_hex(proof_hex, 64),
         {:ok, entry} <- RelayAnnouncement.verify(announcement),
         true <- entry.public_key == peer_pk,
         true <-
           Identity.verify(
             peer_pk,
             proof_message(
               state.identity.public_key,
               peer_pk,
               link.client_nonce,
               server_nonce,
               entry.x25519_public
             ),
             proof
           ) do
      session = Session.establish(state.identity, peer_pk, entry.x25519_public)

      link = %{
        link
        | stage: :ready,
          server_nonce: server_nonce,
          channel: channel(link.client_nonce, server_nonce),
          session: session,
          received_seq: -1
      }

      cancel_handshake_timer(link)
      state = put_link(state, peer_pk, Map.put(link, :handshake_timer, nil))

      case send_message(state, peer_pk, link, @kind_ready, nil, nil) do
        {:ok, state} -> announce_peer_up(state, peer_pk)
        {:error, _} -> peer_down(state, peer_pk)
      end
    else
      _ -> peer_down(state, peer_pk)
    end
  rescue
    _ -> peer_down(state, peer_pk)
  end

  defp handle_link_frame(state, peer_pk, %{stage: :proof_sent} = link, payload),
    do: decrypt_first(state, peer_pk, link, payload)

  defp handle_link_frame(state, peer_pk, %{stage: :ready} = link, payload),
    do: decrypt_ready(state, peer_pk, link, payload)

  defp handle_link_frame(state, _peer_pk, _link, _payload), do: state

  defp decrypt_first(state, peer_pk, link, payload) do
    with {:ok, decoded} <- Packet.decode(payload),
         true <-
           decoded.src == peer_pk and decoded.dst == state.identity.public_key and
             decoded.seq == 0,
         true <- is_binary(decoded.ek) and byte_size(decoded.ek) == 32,
         session = Session.accept(state.identity, peer_pk, decoded.ek, decoded.session_id),
         {:ok, plaintext} <- Session.decrypt(session, decoded.nonce, decoded.ciphertext),
         {:ok, kind, request_id, body} <- decode_plaintext(link.channel, plaintext),
         true <- kind in [@kind_request, @kind_forward, @kind_ready, @kind_route] do
      cancel_handshake_timer(link)
      link = %{link | stage: :ready, session: session, received_seq: 0, handshake_timer: nil}
      state = put_link(state, peer_pk, link)
      state = announce_peer_up(state, peer_pk)
      dispatch_plaintext(state, peer_pk, kind, request_id, body)
    else
      _ -> peer_down(state, peer_pk)
    end
  rescue
    _ -> peer_down(state, peer_pk)
  end

  defp decrypt_ready(state, peer_pk, link, payload) do
    with {:ok, decoded} <- Packet.decode(payload),
         true <- decoded.src == peer_pk and decoded.dst == state.identity.public_key,
         true <- decoded.session_id == link.session.session_id,
         true <- decoded.seq == link.received_seq + 1,
         {:ok, plaintext} <- Session.decrypt(link.session, decoded.nonce, decoded.ciphertext),
         {:ok, kind, request_id, body} <- decode_plaintext(link.channel, plaintext) do
      state = put_link(state, peer_pk, %{link | received_seq: decoded.seq})
      dispatch_plaintext(state, peer_pk, kind, request_id, body)
    else
      _ -> peer_down(state, peer_pk)
    end
  rescue
    _ -> peer_down(state, peer_pk)
  end

  defp dispatch_plaintext(state, peer_pk, @kind_request, request_id, request)
       when is_binary(request_id) and is_map(request) do
    relay = state.relay
    manager = self()

    callback_key = {peer_pk, request_id}
    channel = state.links |> Map.fetch!(peer_pk) |> Map.fetch!(:channel)

    if map_size(state.callbacks) < @max_inbound_callbacks and
         Enum.count(state.callbacks, fn {_key, callback} -> callback.peer == peer_pk end) <
           @max_inbound_callbacks_per_peer and
         not Map.has_key?(state.callbacks, callback_key) do
      {:ok, pid} =
        Task.Supervisor.start_child(Arc.Net.TaskSupervisor, fn ->
          response =
            try do
              Relay.federation_request(relay, peer_pk, request)
            rescue
              _ -> %{"error" => "unavailable"}
            catch
              _, _ -> %{"error" => "unavailable"}
            end

          send(
            manager,
            {:callback_reply, callback_key, peer_pk, request_id, channel,
             normalize_response(response)}
          )
        end)

      %{
        state
        | callbacks:
            Map.put(state.callbacks, callback_key, %{
              peer: peer_pk,
              channel: channel,
              pid: pid,
              ref: Process.monitor(pid)
            })
      }
    else
      peer_down(state, peer_pk)
    end
  end

  defp dispatch_plaintext(state, peer_pk, @kind_response, request_id, response)
       when is_binary(request_id) and is_map(response),
       do: complete_peer_pending(state, peer_pk, request_id, response)

  defp dispatch_plaintext(state, peer_pk, @kind_forward, nil, packet)
       when is_binary(packet) and byte_size(packet) <= @max_forward_bytes do
    Relay.federation_packet(state.relay, peer_pk, packet)
    state
  end

  defp dispatch_plaintext(state, peer_pk, @kind_route, nil, encoded) when is_binary(encoded) do
    case FederationRoute.decode(encoded) do
      {:ok, route} -> Relay.federation_routed_packet(state.relay, peer_pk, route)
      _ -> :ok
    end

    state
  end

  defp dispatch_plaintext(state, _peer_pk, @kind_ready, nil, nil), do: state

  defp dispatch_plaintext(state, _peer_pk, _kind, _request_id, _body), do: state

  defp send_message(state, peer_pk, link, kind, request_id, body) do
    with {:ok, plaintext} <- encode_plaintext(link.channel, kind, request_id, body),
         {nonce, ciphertext, seq, session} <- Session.encrypt(link.session, plaintext),
         packet <-
           Packet.encode(state.identity, peer_pk, session.session_id, seq, nonce, ciphertext,
             ek: session.ek_pub
           ),
         :ok <- Connection.send_federation(link.conn, packet) do
      {:ok, put_link(state, peer_pk, %{link | session: session})}
    else
      {:error, reason} -> {:error, reason}
      _ -> {:error, :federation_peer_unavailable}
    end
  rescue
    _ -> {:error, :federation_peer_unavailable}
  end

  defp encode_plaintext(channel, kind, request_id, body)
       when kind in [@kind_request, @kind_response] and is_binary(request_id) and
              byte_size(request_id) == 16 and is_map(body) do
    encoded = json(body)

    if byte_size(encoded) <= @max_request_bytes,
      do: {:ok, <<@version, kind, channel::binary, request_id::binary, encoded::binary>>},
      else: {:error, :request_too_large}
  rescue
    _ -> {:error, :invalid_federation_request}
  end

  defp encode_plaintext(channel, @kind_forward, nil, body)
       when is_binary(body) and byte_size(body) <= @max_forward_bytes,
       do: {:ok, <<@version, @kind_forward, channel::binary, body::binary>>}

  defp encode_plaintext(channel, @kind_route, nil, body)
       when is_binary(body) and byte_size(body) <= @max_route_bytes,
       do: {:ok, <<@version, @kind_route, channel::binary, body::binary>>}

  defp encode_plaintext(channel, @kind_ready, nil, nil),
    do: {:ok, <<@version, @kind_ready, channel::binary>>}

  defp encode_plaintext(_, _, _, _), do: {:error, :invalid_federation_request}

  defp decode_plaintext(
         channel,
         <<@version, kind, received_channel::binary-size(32), request_id::binary-size(16),
           body::binary>>
       )
       when kind in [@kind_request, @kind_response] and received_channel == channel and
              byte_size(body) <= @max_request_bytes do
    case safe_json(body) do
      {:ok, map} when is_map(map) -> {:ok, kind, request_id, map}
      _ -> :error
    end
  end

  defp decode_plaintext(
         channel,
         <<@version, @kind_forward, received_channel::binary-size(32), packet::binary>>
       )
       when received_channel == channel and byte_size(packet) <= @max_forward_bytes,
       do: {:ok, @kind_forward, nil, packet}

  defp decode_plaintext(
         channel,
         <<@version, @kind_route, received_channel::binary-size(32), route::binary>>
       )
       when received_channel == channel and byte_size(route) <= @max_route_bytes,
       do: {:ok, @kind_route, nil, route}

  defp decode_plaintext(channel, <<@version, @kind_ready, received_channel::binary-size(32)>>)
       when received_channel == channel,
       do: {:ok, @kind_ready, nil, nil}

  defp decode_plaintext(_, _), do: :error

  defp peer_down(state, peer_pk) do
    state =
      case Map.pop(state.links, peer_pk) do
        {nil, _} ->
          state

        {link, links} ->
          cancel_handshake_timer(link)
          stop_owned_task(Map.get(link, :dial_pid), Map.get(link, :dial_ref))
          if Map.get(link, :conn_ref), do: Process.demonitor(link.conn_ref, [:flush])

          if is_pid(Map.get(link, :conn)) and Process.alive?(link.conn),
            do: Connection.close(link.conn)

          %{state | links: links}
      end

    state =
      Enum.reduce(state.pending, state, fn {request_id, pending}, acc ->
        if pending.peer == peer_pk,
          do: complete_pending(acc, request_id, {:error, :federation_peer_unavailable}),
          else: acc
      end)

    callbacks =
      Map.reject(state.callbacks, fn {_key, callback} ->
        if callback.peer == peer_pk do
          stop_owned_task(callback.pid, callback.ref)
          true
        else
          false
        end
      end)

    state = %{state | callbacks: callbacks}

    send(state.relay, {:federation_peer_down, peer_pk})
    schedule_connect(state, peer_pk)
  end

  defp announce_peer_up(state, peer_pk) do
    case Map.get(state.links, peer_pk) do
      %{stage: :ready, announced_up: true} ->
        state

      %{stage: :ready} = link ->
        send(state.relay, {:federation_peer_up, peer_pk})
        put_link(state, peer_pk, Map.put(link, :announced_up, true))

      _ ->
        state
    end
  end

  defp complete_pending(state, request_id, result) do
    case Map.pop(state.pending, request_id) do
      {nil, _} ->
        state

      {%{from: from, timer: timer}, pending} ->
        Process.cancel_timer(timer, async: true, info: false)
        GenServer.reply(from, result)
        %{state | pending: pending}
    end
  end

  defp complete_peer_pending(state, peer_pk, request_id, response) do
    case Map.get(state.pending, request_id) do
      %{peer: ^peer_pk} -> complete_pending(state, request_id, {:ok, response})
      _ -> state
    end
  end

  defp ready_link(state, peer_pk) do
    case Map.get(state.links, peer_pk) do
      link when is_map(link) ->
        if(ready?(link), do: {:ok, link}, else: {:error, :federation_peer_unavailable})

      _ ->
        {:error, :federation_peer_unavailable}
    end
  end

  defp ready?(%{stage: :ready, conn: conn, session: %Session{}}), do: Process.alive?(conn)
  defp ready?(_), do: false

  defp put_link(state, peer_pk, link), do: %{state | links: Map.put(state.links, peer_pk, link)}

  defp link_for_conn(state, conn),
    do: Enum.find(state.links, fn {_peer, link} -> Map.get(link, :conn) == conn end)

  defp stop_owned_task(pid, ref) do
    if is_reference(ref), do: Process.demonitor(ref, [:flush])
    if is_pid(pid) and Process.alive?(pid), do: Process.exit(pid, :shutdown)
    :ok
  end

  defp schedule_connect(state, peer_pk) do
    if should_dial?(state.identity.public_key, peer_pk) do
      Process.send_after(self(), {:connect_peer, peer_pk}, @reconnect_ms)
    end

    state
  end

  defp handshake_timer(peer_pk, conn),
    do: Process.send_after(self(), {:handshake_timeout, peer_pk, conn}, @handshake_timeout)

  defp cancel_handshake_timer(%{handshake_timer: timer}) when is_reference(timer),
    do: Process.cancel_timer(timer, async: true, info: false)

  defp cancel_handshake_timer(_), do: :ok

  defp recv_hello(socket) do
    with {:ok, hello} <- :gen_tcp.recv(socket, 64, @connect_timeout),
         {:ok, info} <- recv_info(socket),
         {:ok, relay_pk, challenge} <- Handshake.decode_relay_hello(hello) do
      {:ok, relay_pk, challenge, info}
    end
  end

  defp recv_info(socket) do
    with {:ok, <<len::32-big>>} <- :gen_tcp.recv(socket, 4, @connect_timeout),
         true <- len <= 1024,
         {:ok, body} <- :gen_tcp.recv(socket, len, @connect_timeout),
         {:ok, _} = result <- Handshake.decode_relay_info(body) do
      result
    else
      _ -> {:error, :invalid_relay_info}
    end
  end

  defp normalize_peers(peers) when is_list(peers) and length(peers) <= @max_peers do
    peers
    |> Enum.reduce_while({:ok, %{}}, fn peer, {:ok, acc} ->
      with %{public_key: pk, host: host, port: port} <- peer,
           true <- valid_peer_key?(pk) and is_integer(port) and port in 1..65_535,
           {:ok, _} <- host_charlist(host),
           false <- Map.has_key?(acc, pk) do
        {:cont, {:ok, Map.put(acc, pk, %{host: host, port: port})}}
      else
        _ -> {:halt, {:error, :invalid_federation_peers}}
      end
    end)
  end

  defp normalize_peers(_), do: {:error, :invalid_federation_peers}
  defp host_charlist(host) when is_binary(host), do: {:ok, String.to_charlist(host)}
  defp host_charlist(host) when is_list(host), do: {:ok, host}
  defp host_charlist(_), do: {:error, :invalid_host}
  defp valid_peer_key?(<<_::binary-size(32)>>), do: true
  defp valid_peer_key?(_), do: false
  defp should_dial?(my_pk, peer_pk), do: my_pk < peer_pk

  defp safe_json(bytes) do
    {:ok, :json.decode(bytes)}
  rescue
    _ -> :error
  end

  defp json(map), do: map |> :json.encode() |> IO.iodata_to_binary()
  defp map_bytes(map), do: map |> json() |> byte_size()
  defp hex(binary), do: Base.encode16(binary, case: :lower)

  defp decode_hex(value, bytes) when is_binary(value) do
    case Base.decode16(value, case: :mixed) do
      {:ok, decoded} when byte_size(decoded) == bytes -> {:ok, decoded}
      _ -> :error
    end
  end

  defp decode_hex(_, _), do: :error

  defp channel(client_nonce, server_nonce),
    do: :crypto.hash(:sha256, "ARC_FEDERATION_CHANNEL_V1" <> client_nonce <> server_nonce)

  defp proof_message(client_pk, server_pk, client_nonce, server_nonce, server_x),
    do: @prefix <> client_pk <> server_pk <> client_nonce <> server_nonce <> server_x

  defp normalize_response(response) when is_map(response), do: response
  defp normalize_response(_), do: %{"error" => "invalid_response"}

  defp redacted_state(state) do
    links =
      Map.new(state.links, fn {peer, link} ->
        {Base.encode16(peer, case: :lower),
         Map.take(link, [:role, :stage])
         |> Map.put(:session, :redacted)
         |> Map.put(:channel, :redacted)}
      end)

    %{
      relay: state.relay,
      peers: map_size(state.peers),
      links: links,
      pending: map_size(state.pending),
      identity: :redacted
    }
  end
end
