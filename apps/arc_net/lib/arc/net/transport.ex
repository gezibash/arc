defmodule Arc.Net.Transport do
  @moduledoc """
  Outbound relay connection manager for one local ARC identity.

  Responsibilities:
    - Maintain at most one outbound TCP connection to a relay node for one
      source identity.
    - Deliver packets: if relay is connected, forward via TCP; else fall back
      to the file-based mailbox.
    - Route inbound packets from the relay to local agents via AgentRegistry.

  Inbound delivery trigger: `send(transport_pid, {:deliver, to_pk, packet})`
  Inbound packet notification: `Transport.packet_received/2` (called by Connection)
  """

  use GenServer

  alias Arc.Data.Mailbox
  alias Arc.Data.Packet
  alias Arc.Identity
  alias Arc.Net.Connection
  alias Arc.Net.Handshake
  alias Arc.Net.Telemetry

  @ed25519_pubkey_bytes 32
  @relay_challenge_bytes 32
  @default_relay_hello_timeout_ms 2_000
  @directory_timeout_ms 2_000
  @max_directory_pending 32

  # --- Client API ---

  def start_link(opts \\ []) when is_list(opts) do
    case Keyword.get(opts, :name) do
      nil -> GenServer.start_link(__MODULE__, [])
      name -> GenServer.start_link(__MODULE__, [], name: name)
    end
  end

  @doc "Open an outbound TCP connection to a relay and authenticate with identity proof."
  def connect_relay(transport, host, port, my_identity, expected_relay_pubkey \\ nil) do
    GenServer.call(transport, {:connect_relay, host, port, my_identity, expected_relay_pubkey})
  end

  @doc "Called by Connection when a framed packet arrives from the relay."
  def packet_received(transport_pid, packet) do
    send(transport_pid, {:packet_received, packet})
  end

  @doc false
  def directory_received(transport_pid, payload) when is_binary(payload) do
    send(transport_pid, {:directory_received, payload})
  end

  @doc false
  def directory_request(transport_pid, operation, fields)
      when is_atom(operation) and is_map(fields) do
    GenServer.call(
      transport_pid,
      {:directory_request, operation, fields},
      directory_timeout(operation) + 500
    )
  catch
    :exit, _ -> {:error, :relay_discovery_unavailable}
  end

  @doc false
  def deliver_strict(transport_pid, to_pk, packet) when is_binary(to_pk) and is_binary(packet) do
    GenServer.call(transport_pid, {:deliver_strict, to_pk, packet})
  catch
    :exit, _ -> {:error, :relay_not_connected}
  end

  # --- GenServer Callbacks ---

  @doc false
  def relay_public_key(transport_pid) do
    GenServer.call(transport_pid, :relay_public_key)
  catch
    :exit, _ -> {:error, :relay_not_connected}
  end

  @impl GenServer
  def init([]) do
    {:ok,
     %{
       relay_conn: nil,
       relay_conn_ref: nil,
       my_pubkey: nil,
       relay_target: nil,
       relay_pubkey: nil,
       directory_pending: %{}
     }}
  end

  @impl GenServer
  def handle_call(:relay_public_key, _from, state) do
    if is_pid(state.relay_conn) and Process.alive?(state.relay_conn) do
      {:reply, {:ok, state.relay_pubkey}, state}
    else
      {:reply, {:error, :relay_not_connected}, state}
    end
  end

  def handle_call({:connect_relay, host, port, my_identity, expected_relay_pubkey}, _from, state) do
    cond do
      not valid_identity?(my_identity) ->
        emit([:transport, :connect, :failed], %{count: 1}, %{reason: :invalid_identity})
        {:reply, {:error, :invalid_identity}, state}

      not valid_relay_pin?(expected_relay_pubkey) ->
        emit([:transport, :connect, :failed], %{count: 1}, %{reason: :invalid_relay_pubkey_pin})
        {:reply, {:error, :invalid_relay_pubkey_pin}, state}

      not valid_port?(port) ->
        emit([:transport, :connect, :failed], %{count: 1}, %{reason: :invalid_port})
        {:reply, {:error, :invalid_port}, state}

      true ->
        case normalize_host(host) do
          {:ok, host_cl} ->
            target = {host_cl, port}
            my_pubkey = my_identity.public_key

            if same_live_connection?(state, target, my_pubkey, expected_relay_pubkey) do
              emit([:transport, :connect, :noop], %{count: 1}, %{reason: :already_connected})
              {:reply, :ok, state}
            else
              state = disconnect_relay(state, :reconnect)

              connect_new_relay(
                state,
                host_cl,
                port,
                my_identity,
                target,
                expected_relay_pubkey
              )
            end

          :error ->
            emit([:transport, :connect, :failed], %{count: 1}, %{reason: :invalid_host})
            {:reply, {:error, :invalid_host}, state}
        end
    end
  end

  def handle_call({:deliver_strict, _to_pk, packet}, _from, state) do
    reply =
      case Packet.decode(packet) do
        {:ok, %{src: src}} when src == state.my_pubkey -> strict_send_packet(state, packet)
        _ -> {:error, :invalid_source}
      end

    {:reply, reply, state}
  end

  def handle_call({:directory_request, _operation, _fields}, _from, %{relay_conn: nil} = state) do
    {:reply, {:error, :relay_not_connected}, state}
  end

  def handle_call({:directory_request, operation, fields}, from, state) do
    cond do
      not valid_directory_operation?(operation) ->
        {:reply, {:error, :invalid_directory_request}, state}

      map_size(state.directory_pending) >= @max_directory_pending ->
        {:reply, {:error, :relay_discovery_busy}, state}

      not is_pid(state.relay_conn) or not Process.alive?(state.relay_conn) ->
        {:reply, {:error, :relay_not_connected}, state}

      true ->
        start_directory_request(state, operation, fields, from)
    end
  end

  defp connect_new_relay(state, host_cl, port, my_identity, target, expected_relay_pubkey) do
    case :gen_tcp.connect(host_cl, port, [:binary, packet: :raw, active: false, keepalive: true]) do
      {:ok, socket} ->
        connect_socket(state, socket, my_identity, target, expected_relay_pubkey)

      {:error, reason} ->
        emit([:transport, :connect, :failed], %{count: 1}, %{reason: reason})
        {:reply, {:error, reason}, state}
    end
  end

  defp connect_socket(state, socket, my_identity, target, expected_relay_pubkey) do
    with {:ok, relay_pubkey, relay_challenge} <- recv_relay_hello(socket),
         :ok <- validate_relay_pubkey_pin(relay_pubkey, expected_relay_pubkey),
         {:ok, client_hello, my_pubkey} <-
           Handshake.client_hello(my_identity, relay_pubkey, relay_challenge),
         :ok <- :gen_tcp.send(socket, client_hello) do
      case Connection.start_link(socket: socket, role: :client, transport_pid: self()) do
        {:ok, conn} ->
          case :gen_tcp.controlling_process(socket, conn) do
            :ok ->
              Connection.activate(conn)
              ref = Process.monitor(conn)

              new_state = %{
                state
                | relay_conn: conn,
                  relay_conn_ref: ref,
                  my_pubkey: my_pubkey,
                  relay_target: target,
                  relay_pubkey: relay_pubkey
              }

              emit([:transport, :connect, :ok], %{count: 1}, %{target: target})
              {:reply, :ok, new_state}

            {:error, reason} ->
              Connection.close(conn)
              :gen_tcp.close(socket)
              emit([:transport, :connect, :failed], %{count: 1}, %{reason: reason})
              {:reply, {:error, reason}, state}
          end

        {:error, reason} ->
          :gen_tcp.close(socket)
          emit([:transport, :connect, :failed], %{count: 1}, %{reason: reason})
          {:reply, {:error, reason}, state}
      end
    else
      {:error, :relay_pubkey_mismatch} ->
        :gen_tcp.close(socket)
        emit([:transport, :connect, :failed], %{count: 1}, %{reason: :relay_pubkey_mismatch})
        {:reply, {:error, :relay_pubkey_mismatch}, state}

      {:error, reason} ->
        :gen_tcp.close(socket)
        emit([:transport, :connect, :failed], %{count: 1}, %{reason: reason})
        {:reply, {:error, reason}, state}
    end
  end

  @impl GenServer
  def handle_info({:deliver, to_pk, packet}, state) do
    case state.relay_conn do
      nil ->
        Mailbox.deliver(to_pk, packet)

        emit([:transport, :deliver, :fallback], %{count: 1, bytes: byte_size(packet)}, %{
          reason: :no_relay
        })

        {:noreply, state}

      conn ->
        if Process.alive?(conn) do
          case safe_send_packet(conn, packet) do
            :ok ->
              {:noreply, state}

            {:error, _} ->
              Mailbox.deliver(to_pk, packet)

              emit([:transport, :deliver, :fallback], %{count: 1, bytes: byte_size(packet)}, %{
                reason: :relay_send_failed
              })

              {:noreply, disconnect_relay(state, :relay_send_failed)}
          end
        else
          Mailbox.deliver(to_pk, packet)

          emit([:transport, :deliver, :fallback], %{count: 1, bytes: byte_size(packet)}, %{
            reason: :relay_dead
          })

          {:noreply, disconnect_relay(state, :relay_dead)}
        end
    end
  end

  def handle_info({:packet_received, packet}, state) do
    case Packet.decode(packet) do
      {:ok, %{dst: dst_pk}} ->
        case Registry.lookup(Arc.Data.AgentRegistry, dst_pk) do
          [{pid, _}] ->
            send(pid, {:arc_packet, packet})

            emit(
              [:transport, :packet, :delivered_local],
              %{count: 1, bytes: byte_size(packet)},
              %{}
            )

          [] ->
            :ok
        end

      {:error, _} ->
        emit([:transport, :packet, :dropped], %{count: 1, bytes: byte_size(packet)}, %{
          reason: :invalid_packet
        })

        :ok
    end

    {:noreply, state}
  end

  def handle_info({:directory_received, payload}, state) do
    state =
      case decode_directory_reply(payload) do
        {:ok, request_id, reply} -> complete_directory_request(state, request_id, {:ok, reply})
        :error -> state
      end

    {:noreply, state}
  end

  def handle_info({:directory_timeout, request_id}, state) do
    {:noreply,
     complete_directory_request(state, request_id, {:error, :relay_discovery_unavailable})}
  end

  def handle_info({:DOWN, ref, :process, _pid, _reason}, %{relay_conn_ref: ref} = state) do
    emit([:transport, :relay, :down], %{count: 1}, %{})

    state = fail_directory_requests(state, :relay_discovery_unavailable)

    {:noreply,
     %{state | relay_conn: nil, relay_conn_ref: nil, relay_target: nil, relay_pubkey: nil}}
  end

  def handle_info(_msg, state), do: {:noreply, state}

  defp strict_send_packet(state, packet) do
    case state.relay_conn do
      conn when is_pid(conn) ->
        if Process.alive?(conn) do
          case safe_send_packet(conn, packet) do
            :ok -> :ok
            {:error, reason} -> {:error, reason}
          end
        else
          {:error, :relay_not_connected}
        end

      _ ->
        {:error, :relay_not_connected}
    end
  end

  defp start_directory_request(state, operation, fields, from) do
    request_id = Base.encode16(:crypto.strong_rand_bytes(16), case: :lower)

    request =
      fields |> Map.put("type", Atom.to_string(operation)) |> Map.put("request_id", request_id)

    case safe_send_control(state.relay_conn, request) do
      :ok ->
        timer =
          Process.send_after(
            self(),
            {:directory_timeout, request_id},
            directory_timeout(operation)
          )

        pending = Map.put(state.directory_pending, request_id, %{from: from, timer: timer})
        {:noreply, %{state | directory_pending: pending}}

      {:error, _reason} ->
        {:reply, {:error, :relay_discovery_unavailable}, state}
    end
  end

  defp same_live_connection?(state, target, my_pubkey, expected_relay_pubkey) do
    state.relay_conn != nil and
      Process.alive?(state.relay_conn) and
      state.relay_target == target and
      state.my_pubkey == my_pubkey and
      relay_pin_matches?(state.relay_pubkey, expected_relay_pubkey)
  end

  defp safe_send_packet(conn, packet) do
    Connection.send_packet(conn, packet)
  catch
    :exit, _ -> {:error, :relay_not_connected}
  end

  defp safe_send_control(conn, request) do
    Connection.send_control(conn, request)
  catch
    :exit, _ -> {:error, :relay_not_connected}
  end

  defp disconnect_relay(state, reason) do
    if state.relay_conn_ref do
      Process.demonitor(state.relay_conn_ref, [:flush])
    end

    if state.relay_conn && Process.alive?(state.relay_conn) do
      Connection.close(state.relay_conn)
    end

    if state.relay_conn do
      emit([:transport, :relay, :disconnected], %{count: 1}, %{reason: reason})
    end

    state = fail_directory_requests(state, :relay_discovery_unavailable)
    %{state | relay_conn: nil, relay_conn_ref: nil, relay_target: nil, relay_pubkey: nil}
  end

  defp valid_directory_operation?(operation), do: operation in [:announce, :search, :resolve]

  defp directory_timeout(operation) when operation in [:search, :resolve], do: 10_000
  defp directory_timeout(_), do: @directory_timeout_ms

  defp decode_directory_reply(payload) do
    case :json.decode(payload) do
      %{"type" => "reply", "request_id" => request_id} = reply
      when is_binary(request_id) and byte_size(request_id) == 32 ->
        {:ok, request_id, reply}

      _ ->
        :error
    end
  rescue
    _ -> :error
  end

  defp complete_directory_request(state, request_id, result) do
    case Map.pop(state.directory_pending, request_id) do
      {nil, _} ->
        state

      {%{from: from, timer: timer}, pending} ->
        Process.cancel_timer(timer, async: true, info: false)
        GenServer.reply(from, result)
        %{state | directory_pending: pending}
    end
  end

  defp fail_directory_requests(state, reason) do
    Enum.each(state.directory_pending, fn {_id, %{from: from, timer: timer}} ->
      Process.cancel_timer(timer, async: true, info: false)
      GenServer.reply(from, {:error, reason})
    end)

    %{state | directory_pending: %{}}
  end

  defp valid_pubkey?(pubkey) when is_binary(pubkey),
    do: byte_size(pubkey) == @ed25519_pubkey_bytes

  defp valid_pubkey?(_), do: false

  defp valid_identity?(%Identity{public_key: pubkey, secret_key: secret_key}) do
    valid_pubkey?(pubkey) and is_binary(secret_key) and byte_size(secret_key) > 0
  end

  defp valid_identity?(_), do: false

  defp valid_relay_pin?(nil), do: true
  defp valid_relay_pin?(pubkey), do: valid_pubkey?(pubkey)

  defp valid_port?(port) when is_integer(port), do: port > 0 and port < 65_536
  defp valid_port?(_), do: false

  defp normalize_host(host) when is_binary(host) and host != "",
    do: {:ok, String.to_charlist(host)}

  defp normalize_host(host) when is_list(host) and host != [], do: {:ok, host}
  defp normalize_host(_), do: :error

  defp emit(event_suffix, measurements, metadata) do
    Telemetry.execute(event_suffix, measurements, metadata)
  end

  defp recv_relay_hello(socket) do
    timeout =
      Application.get_env(:arc_net, :relay_hello_timeout_ms, @default_relay_hello_timeout_ms)

    relay_hello_bytes = @ed25519_pubkey_bytes + @relay_challenge_bytes

    with {:ok, relay_hello} <- :gen_tcp.recv(socket, relay_hello_bytes, timeout),
         {:ok, relay_pubkey, relay_challenge} <- Handshake.decode_relay_hello(relay_hello) do
      {:ok, relay_pubkey, relay_challenge}
    else
      {:error, reason} -> {:error, reason}
    end
  end

  defp validate_relay_pubkey_pin(relay_pubkey, nil) when is_binary(relay_pubkey), do: :ok

  defp validate_relay_pubkey_pin(relay_pubkey, expected_relay_pubkey) do
    if relay_pubkey == expected_relay_pubkey do
      :ok
    else
      {:error, :relay_pubkey_mismatch}
    end
  end

  defp relay_pin_matches?(_relay_pubkey, nil), do: true

  defp relay_pin_matches?(relay_pubkey, expected_relay_pubkey),
    do: relay_pubkey == expected_relay_pubkey
end
