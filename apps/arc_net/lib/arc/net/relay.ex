defmodule Arc.Net.Relay do
  @moduledoc """
  TCP relay node. Accepts inbound connections from Arc agents,
  maintains partitioned route ownership, and forwards packets by destination pubkey.

  The relay is a blind forwarder: it verifies packet signatures (via Packet.decode)
  but never sees plaintext. Session payloads remain end-to-end encrypted.

  Per-packet forwarding stays off the relay mailbox via ETS lookups.
  Connection churn (register/replace/disconnect) is handled by partitioned RouteShard
  GenServers so control-plane writes scale with partitions.
  """

  use GenServer

  alias Arc.Data.Packet
  alias Arc.Net.Connection
  alias Arc.Net.Relay.RouteShard
  alias Arc.Net.Telemetry

  @default_backlog 4096
  @default_acceptors max(2, System.schedulers_online())
  @default_route_partitions max(4, System.schedulers_online() * 4)
  @runtime_key {__MODULE__, :runtime}

  # --- Client API ---

  def start_link(port, opts \\ []) when is_integer(port) and is_list(opts) do
    GenServer.start_link(__MODULE__, {port, opts}, name: __MODULE__)
  end

  @doc """
  Register a connection's pubkey after handshake completion.

  Fast path dispatches directly to the owning RouteShard.
  """
  def register(relay_pid, conn_pid, pubkey) do
    if Process.alive?(relay_pid) do
      case shard_for_pubkey(pubkey) do
        nil ->
          # Fallback path during shard restarts / runtime swaps.
          GenServer.cast(relay_pid, {:register_fallback, conn_pid, pubkey})

        shard_pid ->
          RouteShard.register(shard_pid, conn_pid, pubkey)
      end
    end

    :ok
  end

  @doc "Route a packet using ETS-backed fast path (no relay mailbox hop)."
  def route_packet(relay_pid, conn_pid, packet)
      when is_pid(relay_pid) and is_pid(conn_pid) and is_binary(packet) do
    if Process.alive?(relay_pid) do
      route_packet_fast(conn_pid, packet)
    else
      :ok
    end
  end

  @doc "Return the port the relay is listening on."
  def get_port(relay_pid) do
    GenServer.call(relay_pid, :get_port)
  end

  @doc "Return the relay public key."
  def get_pubkey(relay_pid) do
    GenServer.call(relay_pid, :get_pubkey)
  end

  @doc "Return aggregated relay stats from all route shards."
  def stats(relay_pid) do
    GenServer.call(relay_pid, :stats)
  end

  @doc "Look up current destination connection pid for a pubkey."
  def route_for(relay_pid, pubkey) do
    if Process.alive?(relay_pid) and valid_pubkey?(pubkey) do
      lookup_route(pubkey)
    else
      nil
    end
  end

  # --- GenServer Callbacks ---

  @impl GenServer
  def init(port) when is_integer(port), do: init({port, []})

  def init({port, opts}) do
    backlog = Application.get_env(:arc_net, :relay_backlog, @default_backlog)
    acceptors = Application.get_env(:arc_net, :relay_acceptors, @default_acceptors)

    route_partitions =
      Application.get_env(:arc_net, :relay_route_partitions, @default_route_partitions)

    relay_public_key = Keyword.get(opts, :relay_public_key, :crypto.strong_rand_bytes(32))

    if valid_pubkey?(relay_public_key) and valid_count?(acceptors) and
         valid_count?(route_partitions) do
      cleanup_previous_runtime()

      {:ok, listen_socket} =
        :gen_tcp.listen(port, [
          :binary,
          packet: :raw,
          active: false,
          reuseaddr: true,
          backlog: backlog
        ])

      routes_tables = init_tables(route_partitions)
      {shard_pids, shard_refs} = start_shards(route_partitions, routes_tables)
      put_runtime(routes_tables, shard_pids)

      {:ok, actual_port} = :inet.port(listen_socket)
      acceptor_refs = start_acceptors(listen_socket, self(), relay_public_key, acceptors)

      state = %{
        listen_socket: listen_socket,
        port: actual_port,
        relay_public_key: relay_public_key,
        acceptor_count: acceptors,
        route_partitions: route_partitions,
        acceptor_refs: acceptor_refs,
        routes_tables: routes_tables,
        shard_pids: shard_pids,
        shard_refs: shard_refs
      }

      emit([:relay, :started], %{count: 1}, %{
        port: actual_port,
        backlog: backlog,
        acceptors: acceptors,
        route_partitions: route_partitions,
        route_shards: map_size(shard_pids)
      })

      {:ok, state}
    else
      reason =
        cond do
          not valid_pubkey?(relay_public_key) -> :invalid_relay_pubkey
          not valid_count?(acceptors) -> :invalid_acceptor_count
          true -> :invalid_route_partitions
        end

      {:stop, reason}
    end
  end

  @impl GenServer
  def handle_call(:get_port, _from, state), do: {:reply, state.port, state}

  def handle_call(:get_pubkey, _from, state), do: {:reply, state.relay_public_key, state}

  def handle_call(:stats, _from, state) do
    shard_stats =
      Enum.reduce(state.shard_pids, %{routes: 0, conns: 0}, fn {_idx, shard_pid}, acc ->
        %{routes: routes, conns: conns} = safe_shard_stats(shard_pid)
        %{routes: acc.routes + routes, conns: acc.conns + conns}
      end)

    stats =
      Map.merge(shard_stats, %{
        acceptors: map_size(state.acceptor_refs),
        shards: map_size(state.shard_pids),
        route_partitions: state.route_partitions
      })

    {:reply, stats, state}
  end

  @impl GenServer
  def handle_cast({:register_fallback, conn_pid, pubkey}, state) do
    dispatch_register(state, conn_pid, pubkey)
    {:noreply, state}
  end

  @impl GenServer
  def handle_info({:DOWN, ref, :process, pid, _reason}, state) do
    if Map.get(state.acceptor_refs, pid) == ref do
      acceptor_refs =
        restart_acceptor(
          state.acceptor_refs,
          pid,
          state.listen_socket,
          self(),
          state.relay_public_key
        )

      emit([:relay, :acceptor, :restarted], %{count: 1}, %{})
      {:noreply, %{state | acceptor_refs: acceptor_refs}}
    else
      case Map.get(state.shard_refs, pid) do
        %{idx: idx, ref: ^ref} ->
          state = restart_shard(state, pid, idx)
          {:noreply, state}

        _ ->
          {:noreply, state}
      end
    end
  end

  def handle_info(_msg, state), do: {:noreply, state}

  @impl GenServer
  def terminate(_reason, state) do
    :gen_tcp.close(state.listen_socket)

    Enum.each(state.acceptor_refs, fn {_pid, ref} ->
      Process.demonitor(ref, [:flush])
    end)

    stop_shards(state.shard_refs)
    delete_tables(state.routes_tables)
    clear_runtime()

    emit([:relay, :stopped], %{count: 1}, %{})
    :ok
  end

  # --- Private ---

  defp route_packet_fast(conn_pid, packet) do
    case Packet.decode(packet) do
      {:ok, %{src: src_pk, dst: dst_pk}} ->
        if valid_sender_fast?(conn_pid, src_pk) do
          forward_fast(lookup_route(dst_pk), packet)
        else
          drop_fast(packet, :invalid_sender)
        end

      {:error, _} ->
        drop_fast(packet, :invalid_packet)
    end
  end

  defp forward_fast(nil, packet), do: drop_fast(packet, :no_route)

  defp forward_fast(dst_conn_pid, packet) do
    case Connection.forward_packet(dst_conn_pid, packet) do
      :ok ->
        emit([:relay, :packet, :forwarded], %{count: 1, bytes: byte_size(packet)}, %{})
        :ok

      {:error, :backpressure} ->
        drop_fast(packet, :backpressure)
    end
  end

  defp drop_fast(packet, reason) do
    emit([:relay, :packet, :dropped], %{count: 1, bytes: byte_size(packet)}, %{reason: reason})
    :ok
  end

  defp valid_sender_fast?(conn_pid, src_pk) do
    lookup_route(src_pk) == conn_pid
  end

  defp lookup_route(dst_pk) do
    case routes_table(dst_pk) do
      nil ->
        nil

      table ->
        case ets_lookup(table, dst_pk) do
          [{^dst_pk, conn_pid}] -> conn_pid
          _ -> nil
        end
    end
  end

  defp dispatch_register(state, conn_pid, pubkey) do
    if is_pid(conn_pid) and valid_pubkey?(pubkey) do
      case shard_pid_from_state(state, pubkey) do
        nil -> :ok
        shard_pid -> RouteShard.register(shard_pid, conn_pid, pubkey)
      end
    end

    :ok
  end

  defp shard_pid_from_state(state, pubkey) do
    idx = :erlang.phash2(pubkey, state.route_partitions)
    Map.get(state.shard_pids, idx)
  end

  defp restart_shard(state, dead_pid, idx) do
    clear_partition_table(idx, state.routes_tables)
    routes_table = Enum.at(state.routes_tables, idx)
    {new_pid, new_ref} = start_shard(idx, routes_table)

    shard_pids = Map.put(state.shard_pids, idx, new_pid)

    shard_refs =
      state.shard_refs
      |> Map.delete(dead_pid)
      |> Map.put(new_pid, %{idx: idx, ref: new_ref})

    put_runtime(state.routes_tables, shard_pids)

    emit([:relay, :shard, :restarted], %{count: 1}, %{idx: idx})
    %{state | shard_pids: shard_pids, shard_refs: shard_refs}
  end

  defp safe_shard_stats(shard_pid) do
    RouteShard.stats(shard_pid)
  catch
    :exit, _ -> %{routes: 0, conns: 0}
  end

  defp init_tables(partitions) do
    Enum.map(1..partitions, fn _ ->
      :ets.new(:arc_net_relay_partition, [
        :set,
        :public,
        read_concurrency: true,
        write_concurrency: true
      ])
    end)
  end

  defp clear_partition_table(idx, routes_tables) do
    clear_table(Enum.at(routes_tables, idx))
    :ok
  end

  defp clear_table(nil), do: :ok

  defp clear_table(table) do
    :ets.delete_all_objects(table)
  catch
    :error, :badarg -> :ok
  end

  defp delete_tables(tables) do
    Enum.each(tables, fn table ->
      try do
        :ets.delete(table)
      catch
        :error, :badarg -> :ok
      end
    end)
  end

  defp start_shards(route_partitions, routes_tables) do
    Enum.reduce(0..(route_partitions - 1), {%{}, %{}}, fn idx, {shard_pids, shard_refs} ->
      routes_table = Enum.at(routes_tables, idx)
      {shard_pid, shard_ref} = start_shard(idx, routes_table)

      shard_pids = Map.put(shard_pids, idx, shard_pid)
      shard_refs = Map.put(shard_refs, shard_pid, %{idx: idx, ref: shard_ref})
      {shard_pids, shard_refs}
    end)
  end

  defp start_shard(idx, routes_table) do
    {:ok, shard_pid} = RouteShard.start_link(index: idx, routes_table: routes_table)
    Process.unlink(shard_pid)
    shard_ref = Process.monitor(shard_pid)
    {shard_pid, shard_ref}
  end

  defp stop_shards(shard_refs) do
    Enum.each(shard_refs, fn {shard_pid, %{ref: ref}} ->
      Process.demonitor(ref, [:flush])

      if Process.alive?(shard_pid) do
        try do
          GenServer.stop(shard_pid, :normal)
        catch
          :exit, _ -> :ok
        end
      end
    end)
  end

  defp start_acceptors(listen_socket, relay_pid, relay_public_key, count) do
    Enum.reduce(1..count, %{}, fn _, acc ->
      {pid, ref} = start_acceptor(listen_socket, relay_pid, relay_public_key)
      Map.put(acc, pid, ref)
    end)
  end

  defp start_acceptor(listen_socket, relay_pid, relay_public_key) do
    {:ok, acceptor_pid} =
      Task.Supervisor.start_child(Arc.Net.TaskSupervisor, fn ->
        accept_loop(listen_socket, relay_pid, relay_public_key)
      end)

    acceptor_ref = Process.monitor(acceptor_pid)
    {acceptor_pid, acceptor_ref}
  end

  defp restart_acceptor(
         acceptor_refs,
         dead_acceptor_pid,
         listen_socket,
         relay_pid,
         relay_public_key
       ) do
    {acceptor_pid, acceptor_ref} = start_acceptor(listen_socket, relay_pid, relay_public_key)
    Map.put(Map.delete(acceptor_refs, dead_acceptor_pid), acceptor_pid, acceptor_ref)
  end

  defp accept_loop(listen_socket, relay_pid, relay_public_key) do
    case :gen_tcp.accept(listen_socket) do
      {:ok, socket} ->
        {:ok, conn} =
          Connection.start_link(
            socket: socket,
            role: :relay_client,
            relay_pid: relay_pid,
            relay_pubkey: relay_public_key
          )

        :ok = :gen_tcp.controlling_process(socket, conn)
        Connection.send_relay_hello(conn)
        Connection.activate(conn)
        accept_loop(listen_socket, relay_pid, relay_public_key)

      {:error, :closed} ->
        :ok

      {:error, _reason} ->
        accept_loop(listen_socket, relay_pid, relay_public_key)
    end
  end

  defp put_runtime(routes_tables, shard_pids) do
    shard_count = map_size(shard_pids)

    shard_tuple =
      if shard_count > 0 do
        0..(shard_count - 1)
        |> Enum.map(&Map.get(shard_pids, &1))
        |> List.to_tuple()
      else
        {}
      end

    :persistent_term.put(@runtime_key, %{
      routes_tables: routes_tables,
      route_partitions: length(routes_tables),
      shard_count: shard_count,
      shard_pids: shard_tuple
    })
  end

  defp clear_runtime do
    :persistent_term.erase(@runtime_key)
  end

  defp cleanup_previous_runtime do
    case :persistent_term.get(@runtime_key, nil) do
      %{routes_tables: routes_tables} ->
        clear_runtime()
        delete_tables(routes_tables)

      _ ->
        :ok
    end
  end

  defp routes_table(key) do
    case :persistent_term.get(@runtime_key, nil) do
      %{routes_tables: tables, route_partitions: partitions} when partitions > 0 ->
        Enum.at(tables, :erlang.phash2(key, partitions))

      _ ->
        nil
    end
  end

  defp shard_for_pubkey(pubkey) when is_binary(pubkey) do
    case :persistent_term.get(@runtime_key, nil) do
      %{shard_count: shard_count, shard_pids: shard_pids} when shard_count > 0 ->
        shard_pid = elem(shard_pids, :erlang.phash2(pubkey, shard_count))

        if is_pid(shard_pid) and Process.alive?(shard_pid) do
          shard_pid
        else
          nil
        end

      _ ->
        nil
    end
  end

  defp shard_for_pubkey(_), do: nil

  defp valid_count?(value) when is_integer(value), do: value > 0
  defp valid_count?(_), do: false

  defp ets_lookup(table, key) do
    :ets.lookup(table, key)
  catch
    :error, :badarg -> []
  end

  defp emit(event_suffix, measurements, metadata) do
    Telemetry.execute(event_suffix, measurements, metadata)
  end

  defp valid_pubkey?(pubkey) when is_binary(pubkey), do: byte_size(pubkey) == 32
  defp valid_pubkey?(_), do: false
end
