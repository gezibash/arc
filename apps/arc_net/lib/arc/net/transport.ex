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
  @default_relay_connect_timeout_ms 2_000
  @default_reconnect_base_ms 250
  @default_reconnect_cap_ms 5_000
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

  @doc false
  def relay_status(transport_pid) when is_pid(transport_pid) do
    GenServer.call(transport_pid, :relay_status, 500)
  catch
    :exit, _ -> {:error, :relay_status_unavailable}
  end

  @doc false
  def relay_endpoint_context(transport_pid) when is_pid(transport_pid) do
    GenServer.call(transport_pid, :relay_endpoint_context)
  catch
    :exit, _ -> {:error, :relay_not_connected}
  end

  @doc false
  def relay_endpoint_current?(transport_pid, conn_pid)
      when is_pid(transport_pid) and is_pid(conn_pid) do
    GenServer.call(transport_pid, {:relay_endpoint_current?, conn_pid})
  catch
    :exit, _ -> false
  end

  @impl GenServer
  def init([]) do
    # Client connections stay linked for supervisor shutdown cleanup. Trapping
    # their exits lets the monitor below turn an ordinary relay loss into a
    # bounded reconnect rather than taking down this transport.
    Process.flag(:trap_exit, true)

    {:ok,
     %{
       relay_conn: nil,
       relay_conn_ref: nil,
       my_pubkey: nil,
       relay_target: nil,
       relay_pubkey: nil,
       relay_config: nil,
       reconnect_timer: nil,
       reconnect_token: nil,
       reconnect_attempt: 0,
       reconnect_worker: nil,
       reconnect_worker_ref: nil,
       directory_pending: %{}
     }}
  end

  @impl GenServer
  def format_status(_reason, [_pdict, state]) do
    config =
      case state.relay_config do
        nil -> nil
        %{target: target, pin: pin} -> %{target: target, pin: pin, identity: :redacted}
      end

    [data: [{"State", %{state | relay_config: config}}]]
  end

  @impl GenServer
  def terminate(_reason, state) do
    _ = cancel_reconnect(state)

    if is_pid(state.relay_conn) and Process.alive?(state.relay_conn) do
      Process.exit(state.relay_conn, :shutdown)
    end

    :ok
  end

  @impl GenServer
  def handle_call(:relay_public_key, _from, state) do
    if is_pid(state.relay_conn) and Process.alive?(state.relay_conn) do
      {:reply, {:ok, state.relay_pubkey}, state}
    else
      {:reply, {:error, :relay_not_connected}, state}
    end
  end

  def handle_call(:relay_status, _from, state) do
    {:reply, {:ok, relay_status_document(state)}, state}
  end

  def handle_call(:relay_endpoint_context, _from, state) do
    {:reply, current_endpoint_context(state), state}
  end

  def handle_call({:relay_endpoint_current?, conn_pid}, _from, state) do
    {:reply, current_endpoint?(state, conn_pid), state}
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
        connect_relay_for_host(state, host, port, my_identity, expected_relay_pubkey)
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

  defp connect_relay_for_host(state, host, port, my_identity, expected_relay_pubkey) do
    case normalize_host(host) do
      {:ok, host_cl} ->
        connect_relay_target(state, host_cl, port, my_identity, expected_relay_pubkey)

      :error ->
        emit([:transport, :connect, :failed], %{count: 1}, %{reason: :invalid_host})
        {:reply, {:error, :invalid_host}, state}
    end
  end

  defp connect_relay_target(state, host, port, my_identity, expected_relay_pubkey) do
    target = {host, port}
    my_pubkey = my_identity.public_key

    case effective_relay_pin(state, target, my_pubkey, expected_relay_pubkey) do
      {:error, :relay_pubkey_pin_changed} ->
        emit([:transport, :connect, :failed], %{count: 1}, %{reason: :relay_pubkey_pin_changed})
        {:reply, {:error, :relay_pubkey_pin_changed}, state}

      {:ok, relay_pin} ->
        connect_relay_with_pin(state, host, port, my_identity, target, relay_pin)
    end
  end

  defp connect_relay_with_pin(state, host, port, my_identity, target, relay_pin) do
    if same_live_connection?(state, target, my_identity.public_key, relay_pin) do
      emit([:transport, :connect, :noop], %{count: 1}, %{reason: :already_connected})
      {:reply, :ok, state}
    else
      state =
        state
        |> cancel_reconnect()
        |> disconnect_relay(:reconnect)
        |> clear_relay_config()

      connect_new_relay(state, host, port, my_identity, target, relay_pin)
    end
  end

  defp connect_new_relay(state, host_cl, port, my_identity, target, expected_relay_pubkey) do
    case establish_relay(host_cl, port, my_identity, target, expected_relay_pubkey, self()) do
      {:ok, conn, relay_pubkey, my_pubkey} ->
        new_state =
          install_connection(
            state,
            conn,
            relay_pubkey,
            my_pubkey,
            my_identity,
            target,
            expected_relay_pubkey
          )

        emit([:transport, :connect, :ok], %{count: 1}, %{target: target})
        {:reply, :ok, new_state}

      {:error, reason} ->
        emit([:transport, :connect, :failed], %{count: 1}, %{reason: reason})
        {:reply, {:error, reason}, state}
    end
  end

  defp establish_relay(host, port, my_identity, _target, expected_relay_pubkey, transport_pid) do
    case :gen_tcp.connect(host, port, tcp_options(), relay_connect_timeout()) do
      {:ok, socket} ->
        result = establish_socket(socket, my_identity, expected_relay_pubkey, transport_pid)

        if match?({:error, _}, result), do: :gen_tcp.close(socket)
        result

      {:error, reason} ->
        {:error, reason}
    end
  end

  defp establish_socket(socket, my_identity, expected_relay_pubkey, transport_pid) do
    with {:ok, relay_pubkey, relay_challenge} <- recv_relay_hello(socket),
         :ok <- validate_relay_pubkey_pin(relay_pubkey, expected_relay_pubkey),
         {:ok, client_hello, my_pubkey} <-
           Handshake.client_hello(my_identity, relay_pubkey, relay_challenge),
         :ok <- :gen_tcp.send(socket, client_hello) do
      case Connection.start_link(socket: socket, role: :client, transport_pid: transport_pid) do
        {:ok, conn} ->
          # A reconnect worker must not own the established connection. The
          # transport itself keeps its synchronous connection linked.
          if self() != transport_pid, do: Process.unlink(conn)

          case transfer_socket(socket, conn) do
            :ok ->
              Connection.activate(conn)
              {:ok, conn, relay_pubkey, my_pubkey}

            {:error, reason} ->
              Connection.close(conn)
              {:error, reason}
          end

        {:error, reason} ->
          {:error, reason}
      end
    end
  end

  defp transfer_socket(socket, conn) do
    case :gen_tcp.controlling_process(socket, conn) do
      :ok ->
        :ok

      {:error, reason} ->
        Connection.close(conn)
        {:error, reason}
    end
  end

  defp install_connection(
         state,
         conn,
         relay_pubkey,
         my_pubkey,
         my_identity,
         target,
         expected_relay_pubkey
       ) do
    ref = Process.monitor(conn)

    %{
      state
      | relay_conn: conn,
        relay_conn_ref: ref,
        my_pubkey: my_pubkey,
        relay_target: target,
        relay_pubkey: relay_pubkey,
        relay_config: %{
          target: target,
          identity: my_identity,
          pin: expected_relay_pubkey || relay_pubkey
        },
        reconnect_attempt: 0,
        reconnect_worker: nil,
        reconnect_worker_ref: nil
    }
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

              {:noreply, state |> disconnect_relay(:relay_send_failed) |> schedule_reconnect()}
          end
        else
          Mailbox.deliver(to_pk, packet)

          emit([:transport, :deliver, :fallback], %{count: 1, bytes: byte_size(packet)}, %{
            reason: :relay_dead
          })

          {:noreply, state |> disconnect_relay(:relay_dead) |> schedule_reconnect()}
        end
    end
  end

  def handle_info({:packet_received, packet}, state) do
    case Packet.decode(packet) do
      {:ok, %{dst: dst_pk}} ->
        case Registry.lookup(Arc.Data.AgentRegistry, dst_pk) do
          [{pid, _}] ->
            send(pid, {:arc_relay_packet, packet})

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

    {:noreply, relay_lost(state, :connection_down)}
  end

  def handle_info({:reconnect, token}, %{reconnect_token: token, relay_conn: nil} = state) do
    config = state.relay_config
    {host, port} = config.target
    transport = self()

    {worker, worker_ref} =
      spawn_monitor(fn ->
        result =
          establish_relay(host, port, config.identity, config.target, config.pin, transport)

        send(transport, {:relay_reconnect_result, token, result})
      end)

    {:noreply,
     %{state | reconnect_timer: nil, reconnect_worker: worker, reconnect_worker_ref: worker_ref}}
  end

  def handle_info({:reconnect, _token}, state), do: {:noreply, state}

  def handle_info(
        {:relay_reconnect_result, token, {:ok, conn, relay_pubkey, my_pubkey}},
        %{reconnect_token: token, relay_conn: nil, relay_config: config} = state
      ) do
    state =
      install_connection(
        state,
        conn,
        relay_pubkey,
        my_pubkey,
        config.identity,
        config.target,
        config.pin
      )

    emit([:transport, :reconnect, :ok], %{count: 1}, %{target: config.target})
    notify_agent(state.my_pubkey, :up)

    {:noreply,
     %{
       state
       | reconnect_token: nil,
         reconnect_timer: nil,
         reconnect_worker: nil,
         reconnect_worker_ref: nil
     }}
  end

  def handle_info(
        {:relay_reconnect_result, _token, {:ok, conn, _relay_pubkey, _my_pubkey}},
        state
      ) do
    Connection.close(conn)
    {:noreply, state}
  end

  def handle_info(
        {:relay_reconnect_result, token, {:error, reason}},
        %{reconnect_token: token, relay_conn: nil} = state
      ) do
    emit([:transport, :reconnect, :failed], %{count: 1}, %{reason: reason})
    {:noreply, schedule_reconnect(%{state | reconnect_worker: nil, reconnect_worker_ref: nil})}
  end

  def handle_info(
        {:DOWN, ref, :process, _pid, reason},
        %{reconnect_worker_ref: ref, relay_conn: nil} = state
      ) do
    # A worker normally reports its result before exiting. This path handles a
    # crashed worker without blocking the transport or retaining a stale attempt.
    state = %{state | reconnect_worker: nil, reconnect_worker_ref: nil}
    {:noreply, if(reason == :normal, do: state, else: schedule_reconnect(state))}
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
    if state.relay_conn, do: notify_agent(state.my_pubkey, :down)
    %{state | relay_conn: nil, relay_conn_ref: nil}
  end

  defp relay_lost(state, _reason) do
    state = fail_directory_requests(state, :relay_discovery_unavailable)
    notify_agent(state.my_pubkey, :down)

    state
    |> Map.merge(%{relay_conn: nil, relay_conn_ref: nil})
    |> schedule_reconnect()
  end

  defp schedule_reconnect(%{relay_config: nil} = state), do: state

  defp schedule_reconnect(state) do
    state = cancel_reconnect_timer(state)
    token = make_ref()
    delay = reconnect_delay(state.reconnect_attempt)
    timer = Process.send_after(self(), {:reconnect, token}, delay)

    %{
      state
      | reconnect_timer: timer,
        reconnect_token: token,
        reconnect_attempt: state.reconnect_attempt + 1
    }
  end

  defp cancel_reconnect(state) do
    if is_pid(state.reconnect_worker) and Process.alive?(state.reconnect_worker) do
      Process.exit(state.reconnect_worker, :shutdown)
    end

    if state.reconnect_worker_ref do
      Process.demonitor(state.reconnect_worker_ref, [:flush])
    end

    state
    |> cancel_reconnect_timer()
    |> Map.merge(%{
      reconnect_token: nil,
      reconnect_worker: nil,
      reconnect_worker_ref: nil,
      reconnect_attempt: 0
    })
  end

  defp cancel_reconnect_timer(%{reconnect_timer: nil} = state), do: state

  defp cancel_reconnect_timer(state) do
    Process.cancel_timer(state.reconnect_timer, async: true, info: false)
    %{state | reconnect_timer: nil}
  end

  defp clear_relay_config(state) do
    %{state | relay_config: nil, relay_target: nil, relay_pubkey: nil}
  end

  defp reconnect_delay(attempt) do
    base = Application.get_env(:arc_net, :relay_reconnect_base_ms, @default_reconnect_base_ms)
    cap = Application.get_env(:arc_net, :relay_reconnect_cap_ms, @default_reconnect_cap_ms)
    backoff = min(round(max(base, 1) * :math.pow(2, min(attempt, 6))), max(cap, 1))
    jitter = :rand.uniform(max(div(backoff, 4), 1)) - 1
    backoff + jitter
  end

  defp relay_connect_timeout do
    Application.get_env(:arc_net, :relay_connect_timeout_ms, @default_relay_connect_timeout_ms)
  end

  defp relay_status_document(state) do
    status =
      cond do
        is_pid(state.relay_conn) and Process.alive?(state.relay_conn) -> :connected
        is_map(state.relay_config) and reconnecting?(state) -> :reconnecting
        true -> :disconnected
      end

    case state.relay_config do
      %{target: {host, port}} when is_list(host) and is_integer(port) ->
        %{status: status, host: List.to_string(host), port: port}

      _ ->
        %{status: status}
    end
  end

  defp reconnecting?(state) do
    state.reconnect_timer != nil or state.reconnect_token != nil or
      (is_pid(state.reconnect_worker) and Process.alive?(state.reconnect_worker))
  end

  defp tcp_options, do: [:binary, packet: :raw, active: false, keepalive: true, reuseaddr: true]

  defp current_endpoint_context(%{relay_conn: conn, relay_config: %{pin: pin}})
       when is_pid(conn) and is_binary(pin) and byte_size(pin) == @ed25519_pubkey_bytes do
    if Process.alive?(conn) do
      case Connection.endpoint(conn) do
        {:ok, %{local: local}} -> {:ok, %{connection: conn, local: local}}
        {:error, _reason} -> {:error, :relay_not_connected}
      end
    else
      {:error, :relay_not_connected}
    end
  end

  defp current_endpoint_context(_state), do: {:error, :relay_not_connected}

  defp current_endpoint?(%{relay_conn: conn, relay_config: %{pin: pin}}, conn)
       when is_pid(conn) and is_binary(pin) and byte_size(pin) == @ed25519_pubkey_bytes,
       do: Process.alive?(conn)

  defp current_endpoint?(_state, _conn), do: false

  defp effective_relay_pin(state, target, my_pubkey, requested_pin) do
    case state.relay_config do
      %{target: ^target, identity: %{public_key: ^my_pubkey}, pin: stored_pin} ->
        cond do
          requested_pin == nil -> {:ok, stored_pin}
          requested_pin == stored_pin -> {:ok, requested_pin}
          true -> {:error, :relay_pubkey_pin_changed}
        end

      _ ->
        {:ok, requested_pin}
    end
  end

  defp notify_agent(nil, _status), do: :ok

  defp notify_agent(pubkey, status) do
    case Registry.lookup(Arc.Data.AgentRegistry, pubkey) do
      [{pid, _}] -> send(pid, {:arc_relay_status, status})
      [] -> :ok
    end
  end

  defp valid_directory_operation?(operation),
    do: operation in [:announce, :search, :resolve, :observe, :status]

  defp directory_timeout(operation) when operation in [:search, :resolve], do: 10_000
  defp directory_timeout(:status), do: 5_000
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
