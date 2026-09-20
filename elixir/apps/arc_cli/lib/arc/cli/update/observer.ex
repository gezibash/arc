defmodule Arc.CLI.Update.Observer do
  @moduledoc false

  use GenServer

  alias Arc.Data.{Packet, Session}
  alias Arc.Identity
  alias Arc.Net.{Handshake, Relay}

  @tick_ms 50
  @io_timeout 500
  @route_timeout 2_000

  @type result :: %{
          required(String.t()) =>
            String.t() | non_neg_integer() | %{required(String.t()) => term()}
        }

  @doc """
  Start a disposable encrypted traffic probe for a running relay.

  The probe is deliberately independent of ARC's normal local identity and
  transport manager. It uses two fresh identities and raw TCP connections, so
  a successful observation demonstrates that existing relay sockets forwarded
  signed, encrypted packets without reconnecting them.
  """
  @spec start(pid()) :: {:ok, pid()} | {:error, term()}
  def start(relay_pid) when is_pid(relay_pid),
    do: GenServer.start(__MODULE__, {relay_pid, self()})

  def start(_), do: {:error, :invalid_relay}

  @doc """
  Stop the probe and verify that the relay runtime and probe sockets stayed
  intact throughout the observed operation.
  """
  @spec finish(pid()) :: {:ok, result()} | {:error, term()}
  def finish(observer_pid) when is_pid(observer_pid) do
    GenServer.call(observer_pid, :finish, @route_timeout + @io_timeout + 1_000)
  catch
    :exit, {:noproc, _} -> {:error, :observer_not_running}
    :exit, reason -> {:error, {:observer_call_failed, reason}}
  end

  def finish(_), do: {:error, :invalid_observer}

  @doc "Abort an observation and close only its disposable sockets."
  @spec abort(pid()) :: :ok | {:error, term()}
  def abort(observer_pid) when is_pid(observer_pid) do
    if Process.alive?(observer_pid), do: stop_observer(observer_pid)

    :ok
  end

  def abort(_), do: {:error, :invalid_observer}

  defp stop_observer(observer_pid) do
    GenServer.stop(observer_pid, :normal, @io_timeout)
  catch
    :exit, _ -> :ok
  end

  @impl GenServer
  def init({relay_pid, owner}) do
    Process.flag(:trap_exit, true)
    owner_ref = Process.monitor(owner)

    with :ok <- alive(relay_pid),
         {:ok, port} <- safe_relay_call(fn -> Relay.get_port(relay_pid) end),
         {:ok, relay_key} <- safe_relay_call(fn -> Relay.get_pubkey(relay_pid) end),
         {:ok, state} <- connect_probes(relay_pid, port, relay_key) do
      case exchange(state) do
        {:ok, state} ->
          case snapshot(relay_pid) do
            {:ok, runtime} ->
              schedule_tick()
              {:ok, Map.merge(state, %{baseline: runtime, owner_ref: owner_ref})}

            {:error, reason} ->
              close_state_sockets(state)
              {:stop, reason}
          end

        {:error, reason} ->
          close_state_sockets(state)
          {:stop, reason}
      end
    else
      {:error, reason} -> {:stop, reason}
    end
  end

  @impl GenServer
  def handle_info(:tick, %{failure: nil} = state) do
    state =
      case exchange(state) do
        {:ok, next} -> next
        {:error, reason} -> %{state | failure: reason}
      end

    if state.failure == nil, do: schedule_tick()
    {:noreply, state}
  end

  def handle_info(:tick, state), do: {:noreply, state}

  def handle_info({:DOWN, ref, :process, _pid, _reason}, %{owner_ref: ref} = state),
    do: {:stop, :normal, state}

  def handle_info({:EXIT, _pid, reason}, state) when reason not in [:normal, :shutdown],
    do: {:noreply, %{state | failure: {:linked_process_exit, reason}}}

  def handle_info(_message, state), do: {:noreply, state}

  @impl GenServer
  def handle_call(:finish, _from, state) do
    result = verify(state)
    {:stop, :normal, result, state}
  end

  @impl GenServer
  def terminate(_reason, state) do
    close_socket(state[:alice_socket])
    close_socket(state[:bob_socket])
    :ok
  end

  defp connect_probes(relay_pid, port, relay_key) do
    alice = Identity.generate()
    bob = Identity.generate()

    with {:ok, alice_socket} <- relay_connect(port, alice, relay_key) do
      case relay_connect(port, bob, relay_key) do
        {:ok, bob_socket} ->
          case wait_for_routes(relay_pid, alice.public_key, bob.public_key) do
            :ok ->
              {:ok,
               %{
                 relay: relay_pid,
                 alice: alice,
                 bob: bob,
                 alice_socket: alice_socket,
                 bob_socket: bob_socket,
                 alice_to_bob: establish(alice, bob),
                 bob_to_alice: establish(bob, alice),
                 received: %{alice_to_bob: 0, bob_to_alice: 0},
                 failure: nil
               }}

            {:error, _} = error ->
              close_socket(alice_socket)
              close_socket(bob_socket)
              error
          end

        {:error, _} = error ->
          close_socket(alice_socket)
          error
      end
    end
  end

  defp relay_connect(port, identity, expected_relay_key) do
    case :gen_tcp.connect(~c"127.0.0.1", port, [:binary, packet: :raw, active: false]) do
      {:ok, socket} ->
        result =
          with {:ok, relay_hello} <- :gen_tcp.recv(socket, 64, @io_timeout),
               {:ok, relay_key, challenge} <- Handshake.decode_relay_hello(relay_hello),
               :ok <- match_relay_key(relay_key, expected_relay_key),
               {:ok, <<info_length::32-big>>} <- :gen_tcp.recv(socket, 4, @io_timeout),
               :ok <- discard_info(socket, info_length),
               {:ok, hello, _} <- Handshake.client_hello(identity, relay_key, challenge),
               :ok <- :gen_tcp.send(socket, hello) do
            {:ok, socket}
          else
            {:error, reason} -> {:error, {:relay_connection_failed, reason}}
            reason -> {:error, {:relay_connection_failed, reason}}
          end

        case result do
          {:ok, ^socket} = success ->
            success

          {:error, _} = error ->
            close_socket(socket)
            error
        end

      {:error, reason} ->
        {:error, {:relay_connection_failed, reason}}
    end
  end

  defp discard_info(socket, length) when length >= 0 and length <= 1_048_576 do
    case :gen_tcp.recv(socket, length, @io_timeout) do
      {:ok, _info} -> :ok
      {:error, reason} -> {:error, reason}
    end
  end

  defp discard_info(_socket, _length), do: {:error, :invalid_relay_info_length}

  defp match_relay_key(expected, expected), do: :ok
  defp match_relay_key(_actual, _expected), do: {:error, :relay_key_mismatch}

  defp establish(sender, receiver) do
    {:ok, session} = Session.establish(sender, receiver.public_key)
    session
  end

  defp wait_for_routes(relay, alice_key, bob_key) do
    deadline = System.monotonic_time(:millisecond) + @route_timeout
    await_routes(relay, alice_key, bob_key, deadline)
  end

  defp await_routes(relay, alice_key, bob_key, deadline) do
    if is_pid(Relay.route_for(relay, alice_key)) and is_pid(Relay.route_for(relay, bob_key)) do
      :ok
    else
      if System.monotonic_time(:millisecond) < deadline do
        Process.sleep(10)
        await_routes(relay, alice_key, bob_key, deadline)
      else
        {:error, :probe_routes_not_ready}
      end
    end
  end

  defp exchange(state) do
    with {:ok, state} <- send_and_receive(state, :alice_to_bob),
         do: send_and_receive(state, :bob_to_alice)
  end

  defp send_and_receive(state, direction) do
    {sender, receiver, sender_socket, receiver_socket, session} =
      direction_parts(state, direction)

    tag = "arc-update-observer:#{Atom.to_string(direction)}:#{session.seq}"
    {nonce, ciphertext, sequence, next_session} = Session.encrypt(session, tag)

    packet =
      Packet.encode(
        sender,
        receiver.public_key,
        next_session.session_id,
        sequence,
        nonce,
        ciphertext,
        ek: next_session.ek_pub
      )

    with :ok <- :gen_tcp.send(sender_socket, <<byte_size(packet)::32-big, packet::binary>>),
         {:ok, received} <- recv_frame(receiver_socket),
         {:ok, decoded} <- Packet.decode(received),
         :ok <- exact_packet(decoded, sender, receiver, next_session, sequence),
         responder_session <-
           Session.accept(receiver, sender.public_key, decoded.ek, decoded.session_id),
         {:ok, ^tag} <- Session.decrypt(responder_session, decoded.nonce, decoded.ciphertext) do
      {:ok,
       state
       |> Map.put(direction, next_session)
       |> update_in([:received, direction], &(&1 + 1))}
    else
      {:error, reason} -> {:error, {direction, reason}}
      other -> {:error, {direction, other}}
    end
  end

  defp direction_parts(state, :alice_to_bob),
    do: {state.alice, state.bob, state.alice_socket, state.bob_socket, state.alice_to_bob}

  defp direction_parts(state, :bob_to_alice),
    do: {state.bob, state.alice, state.bob_socket, state.alice_socket, state.bob_to_alice}

  defp recv_frame(socket) do
    with {:ok, <<length::32-big>>} <- :gen_tcp.recv(socket, 4, @io_timeout),
         true <- length > 0 and length <= 16 * 1024 * 1024,
         {:ok, received} <- :gen_tcp.recv(socket, length, @io_timeout) do
      {:ok, received}
    else
      false -> {:error, :invalid_frame_length}
      {:error, reason} -> {:error, reason}
      other -> {:error, other}
    end
  end

  defp exact_packet(decoded, sender, receiver, session, sequence) do
    cond do
      decoded.src != sender.public_key -> {:error, :unexpected_source}
      decoded.dst != receiver.public_key -> {:error, :unexpected_destination}
      decoded.seq != sequence -> {:error, :unexpected_sequence}
      decoded.session_id != session.session_id -> {:error, :unexpected_session}
      decoded.ek != session.ek_pub -> {:error, :unexpected_ephemeral_key}
      true -> :ok
    end
  end

  defp snapshot(relay) do
    with :ok <- alive(relay),
         state when is_map(state) <- :sys.get_state(relay),
         true <- state.listen_socket != nil,
         {:ok, port} <- safe_relay_call(fn -> Relay.get_port(relay) end) do
      routes = current_routes(state.routes_tables)

      {:ok,
       %{
         relay: relay,
         port: port,
         socket: state.listen_socket,
         route_tables: state.routes_tables,
         acceptors: state.acceptor_refs |> Map.keys() |> Enum.sort(),
         shards: state.shard_pids |> Map.values() |> Enum.sort(),
         connections: routes
       }}
    else
      false -> {:error, :invalid_relay_runtime}
      {:error, _} = error -> error
      _ -> {:error, :invalid_relay_runtime}
    end
  rescue
    _ -> {:error, :relay_runtime_unavailable}
  end

  defp current_routes(tables) when is_list(tables) do
    tables
    |> Enum.flat_map(fn table ->
      try do
        :ets.tab2list(table)
      rescue
        ArgumentError -> []
      end
    end)
    |> Map.new(fn {public_key, connection_pid} -> {public_key, connection_pid} end)
  end

  defp verify(%{failure: reason}) when not is_nil(reason), do: {:error, {:probe_failed, reason}}

  defp verify(state) do
    with :ok <- minimum_traffic(state.received),
         {:ok, after_runtime} <- snapshot(state.relay),
         :ok <- same_runtime(state.baseline, after_runtime),
         :ok <- probes_still_current(state) do
      {:ok,
       %{
         "verdict" => "passed",
         "port" => state.baseline.port,
         "messages" => %{
           "alice_to_bob" => state.received.alice_to_bob,
           "bob_to_alice" => state.received.bob_to_alice
         },
         "runtime" => runtime_summary(state.baseline)
       }}
    end
  end

  defp minimum_traffic(%{alice_to_bob: left, bob_to_alice: right}) when left >= 1 and right >= 1,
    do: :ok

  defp minimum_traffic(_), do: {:error, :no_bidirectional_probe_traffic}

  defp same_runtime(before, after_runtime) do
    with :ok <- same_runtime_handles(before, after_runtime) do
      same_runtime_processes(before, after_runtime)
    end
  end

  defp same_runtime_handles(before, after_runtime) do
    cond do
      before.relay != after_runtime.relay ->
        {:error, :relay_replaced}

      before.port != after_runtime.port ->
        {:error, :listener_port_changed}

      before.socket != after_runtime.socket ->
        {:error, :listener_socket_changed}

      before.route_tables != after_runtime.route_tables ->
        {:error, :route_tables_changed}

      before.acceptors != after_runtime.acceptors ->
        {:error, :acceptors_changed}

      before.shards != after_runtime.shards ->
        {:error, :route_shards_changed}

      before.connections != after_runtime.connections ->
        {:error, :connections_changed}

      true ->
        :ok
    end
  end

  defp same_runtime_processes(before, after_runtime) do
    cond do
      not Process.alive?(after_runtime.relay) ->
        {:error, :relay_not_alive}

      not Enum.all?(before.acceptors, &Process.alive?/1) ->
        {:error, :acceptor_not_alive}

      not Enum.all?(before.shards, &Process.alive?/1) ->
        {:error, :route_shard_not_alive}

      not Enum.all?(Map.values(before.connections), &Process.alive?/1) ->
        {:error, :connection_not_alive}

      true ->
        :ok
    end
  end

  defp probes_still_current(state) do
    cond do
      Relay.route_for(state.relay, state.alice.public_key) == nil ->
        {:error, :alice_route_missing}

      Relay.route_for(state.relay, state.bob.public_key) == nil ->
        {:error, :bob_route_missing}

      true ->
        :ok
    end
  end

  defp alive(pid) when is_pid(pid) do
    if Process.alive?(pid), do: :ok, else: {:error, :relay_not_alive}
  end

  defp alive(_), do: {:error, :relay_not_alive}

  defp safe_relay_call(fun) do
    {:ok, fun.()}
  catch
    :exit, _ -> {:error, :relay_unavailable}
  end

  defp schedule_tick, do: Process.send_after(self(), :tick, @tick_ms)

  defp close_socket(socket) when is_port(socket) do
    _ = :gen_tcp.close(socket)
    :ok
  end

  defp close_socket(_), do: :ok

  defp close_state_sockets(state) do
    close_socket(state.alice_socket)
    close_socket(state.bob_socket)
  end

  # This is persisted with update journals. Keep process, socket, ETS and
  # connection identities inside the observer only; the durable proof records
  # counts and the successful verdict, never runtime handles.
  defp runtime_summary(runtime) do
    %{
      "acceptors" => length(runtime.acceptors),
      "route_shards" => length(runtime.shards),
      "tracked_connections" => map_size(runtime.connections),
      "route_tables" => length(runtime.route_tables)
    }
  end
end
