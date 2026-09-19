defmodule Arc.Net.Relay do
  @moduledoc """
  TCP relay node. Accepts inbound connections from Arc agents,
  maintains partitioned route ownership, and forwards packets by destination pubkey.

  The relay is a blind forwarder: it verifies packet signatures (via Packet.decode)
  but never sees application plaintext. Session payloads remain end-to-end
  encrypted. Provider announcements and directory queries are intentionally
  public metadata carried on the same authenticated connection.

  Per-packet forwarding stays off the relay mailbox via ETS lookups.
  Connection churn (register/replace/disconnect) is handled by partitioned RouteShard
  GenServers so control-plane writes scale with partitions.
  """

  use GenServer

  alias Arc.Data.Packet
  alias Arc.Data.RelayAnnouncement
  alias Arc.Identity
  alias Arc.Net.Connection
  alias Arc.Net.Federation
  # Kept outside Relay so acceptors do not retain old Relay code during upgrades.
  alias Arc.Net.Relay.Acceptor
  alias Arc.Net.Relay.FederationCatalog
  alias Arc.Net.Relay.FederationDirectory
  alias Arc.Net.Relay.RouteShard
  alias Arc.Net.Telemetry

  @default_backlog 4096
  @default_acceptors max(2, System.schedulers_online())
  @default_route_partitions max(4, System.schedulers_online() * 4)
  @runtime_key {__MODULE__, :runtime}
  @directory_ttl_seconds 180
  @max_directory_records 10_000
  @max_directory_limit 50
  @max_directory_reply_bytes 240 * 1024
  @max_federation_queries 32
  @return_route_ttl 180
  @catalog_interval 2_000
  @max_catalog_syncs 4

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
      route_packet_fast(relay_pid, conn_pid, packet)
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

  @doc false
  def client_authenticated(relay_pid, conn_pid, pubkey)
      when is_pid(relay_pid) and is_pid(conn_pid) and is_binary(pubkey) do
    GenServer.cast(relay_pid, {:client_authenticated, conn_pid, pubkey})
  end

  @doc false
  def directory_frame(relay_pid, conn_pid, pubkey, payload)
      when is_pid(relay_pid) and is_pid(conn_pid) and is_binary(pubkey) and is_binary(payload) do
    GenServer.cast(relay_pid, {:directory_frame, conn_pid, pubkey, payload})
  end

  @doc false
  def federation_frame(relay_pid, conn_pid, pubkey, payload) do
    GenServer.cast(relay_pid, {:federation_frame, conn_pid, pubkey, payload})
  end

  @doc false
  def federation_request(relay_pid, peer, request) do
    timeout = FederationDirectory.request_timeout(request) - 250
    GenServer.call(relay_pid, {:federation_request, peer, request}, timeout)
  end

  @doc false
  def federation_packet(relay_pid, peer, packet) do
    GenServer.cast(relay_pid, {:federation_packet, peer, packet})
  end

  @doc false
  def federation_routed_packet(relay_pid, peer, route) do
    GenServer.cast(relay_pid, {:federation_routed_packet, peer, route})
  end

  # --- GenServer Callbacks ---

  @impl GenServer
  def init(port) when is_integer(port), do: init({port, []})

  def init({port, opts}) do
    case relay_init_config(opts) do
      {:ok, config} -> start_relay(port, config)
      {:error, reason} -> {:stop, reason}
    end
  end

  defp relay_init_config(opts) do
    backlog = Application.get_env(:arc_net, :relay_backlog, @default_backlog)
    acceptors = Application.get_env(:arc_net, :relay_acceptors, @default_acceptors)

    route_partitions =
      Application.get_env(:arc_net, :relay_route_partitions, @default_route_partitions)

    relay_identity = Keyword.get(opts, :relay_identity)
    peers = Keyword.get(opts, :federation_peers, [])
    transit = Keyword.get(opts, :federation_transit, false)
    relay_public_key = relay_public_key(opts, relay_identity)

    with :ok <-
           validate_relay_config(
             relay_public_key,
             acceptors,
             route_partitions,
             relay_identity,
             peers,
             transit
           ) do
      {:ok,
       %{
         backlog: backlog,
         acceptors: acceptors,
         route_partitions: route_partitions,
         relay_identity: relay_identity,
         relay_public_key: relay_public_key,
         peers: peers,
         transit: transit
       }}
    end
  end

  defp relay_public_key(_opts, %Identity{} = identity), do: identity.public_key

  defp relay_public_key(opts, _identity),
    do: Keyword.get(opts, :relay_public_key, :crypto.strong_rand_bytes(32))

  defp validate_relay_config(relay_key, acceptors, route_partitions, identity, peers, transit) do
    cond do
      not valid_pubkey?(relay_key) -> {:error, :invalid_relay_pubkey}
      not valid_count?(acceptors) -> {:error, :invalid_acceptor_count}
      not valid_federation?(identity, peers) -> {:error, :invalid_federation_config}
      not is_boolean(transit) -> {:error, :invalid_federation_config}
      not valid_count?(route_partitions) -> {:error, :invalid_route_partitions}
      true -> :ok
    end
  end

  defp start_relay(port, config) do
    # The route shards stay linked. Trapping exits lets terminate/2 stop them on a parent
    # :shutdown, and a :kill or a failed init still takes the linked shards down.
    Process.flag(:trap_exit, true)
    cleanup_previous_runtime()

    {:ok, listen_socket} =
      :gen_tcp.listen(port, [
        :binary,
        packet: :raw,
        active: false,
        reuseaddr: true,
        backlog: config.backlog
      ])

    routes_tables = init_tables(config.route_partitions)
    {shard_pids, shard_refs} = start_shards(config.route_partitions, routes_tables)
    put_runtime(routes_tables, shard_pids)

    {:ok, actual_port} = :inet.port(listen_socket)

    acceptor_refs =
      start_acceptors(listen_socket, self(), config.relay_public_key, config.acceptors)

    {federation, federation_ref} = start_federation(config.relay_identity, config.peers)
    schedule_federation_work(federation)

    state = %{
      listen_socket: listen_socket,
      port: actual_port,
      relay_public_key: config.relay_public_key,
      acceptor_count: config.acceptors,
      route_partitions: config.route_partitions,
      acceptor_refs: acceptor_refs,
      routes_tables: routes_tables,
      shard_pids: shard_pids,
      shard_refs: shard_refs,
      directory_records: %{},
      directory_clients: %{},
      directory_conn_refs: %{},
      federation: federation,
      federation_ref: federation_ref,
      federation_peers: Enum.map(config.peers, & &1.public_key),
      federation_transit: config.transit,
      federation_queries: %{},
      federation_entries: %{},
      federation_returns: %{},
      federation_network_returns: %{},
      federation_seen: %{},
      federation_catalog:
        FederationCatalog.new(config.relay_public_key, Enum.map(config.peers, & &1.public_key)),
      started_at: System.monotonic_time(:second),
      catalog_generation: 0,
      catalog_ready: MapSet.new(),
      catalog_syncs: %{},
      catalog_queue: MapSet.new(),
      catalog_attempts: %{},
      catalog_hits: 0,
      catalog_sync_pages: 0,
      federation_live_queries: 0
    }

    emit([:relay, :started], %{count: 1}, %{
      port: actual_port,
      backlog: config.backlog,
      acceptors: config.acceptors,
      route_partitions: config.route_partitions,
      route_shards: map_size(shard_pids)
    })

    {:ok, state}
  end

  defp schedule_federation_work(nil), do: :ok

  defp schedule_federation_work(_federation) do
    Process.send_after(self(), :sweep_federation, 30_000)
    Process.send_after(self(), :sync_catalogs, @catalog_interval)
  end

  @impl GenServer
  def handle_call(:get_port, _from, state), do: {:reply, state.port, state}

  def handle_call(:get_pubkey, _from, state), do: {:reply, state.relay_public_key, state}

  def handle_call({:federation_request, peer, request}, from, state) do
    handle_federation_request(state, peer, request, from)
  end

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
        route_partitions: state.route_partitions,
        federation_peers: length(state.federation_peers),
        federation_routes: map_size(state.federation_entries),
        federation_transit: state.federation_transit,
        federation_conversations: map_size(state.federation_network_returns),
        catalog_entries: length(FederationCatalog.entries(state.federation_catalog)),
        catalog_synced_peers: FederationCatalog.synced_peers(state.federation_catalog),
        catalog_hits: state.catalog_hits,
        catalog_sync_pages: state.catalog_sync_pages,
        catalog_syncs: map_size(state.catalog_syncs),
        federation_live_queries: state.federation_live_queries
      })

    {:reply, stats, state}
  end

  @impl GenServer
  def handle_cast({:federation_frame, conn, pubkey, payload}, state) do
    if (state.federation && pubkey in state.federation_peers) and
         federation_queue_available?(state.federation) do
      Federation.inbound(state.federation, conn, pubkey, payload)
    else
      Connection.close(conn)
    end

    {:noreply, state}
  end

  def handle_cast({:federation_packet, peer, packet}, state) do
    {:noreply, receive_federated(state, peer, packet)}
  end

  def handle_cast({:federation_routed_packet, peer, route}, state) do
    {:noreply, receive_routed(state, peer, route)}
  end

  def handle_cast({:route_remote, conn, packet}, state) do
    {:noreply, route_remote(state, conn, packet)}
  end

  def handle_cast({:register_fallback, conn_pid, pubkey}, state) do
    dispatch_register(state, conn_pid, pubkey)
  end

  def handle_cast({:client_authenticated, conn_pid, pubkey}, state) do
    {:noreply, state |> register_directory_client(conn_pid, pubkey) |> refresh_catalog()}
  end

  def handle_cast({:directory_frame, conn_pid, pubkey, payload}, state) do
    state = state |> prune_directory() |> refresh_catalog()

    state =
      if Map.get(state.directory_clients, pubkey) == conn_pid do
        handle_directory_control(state, conn_pid, pubkey, payload)
      else
        state
      end

    {:noreply, refresh_catalog(state)}
  end

  # An acceptor stopped with `:shutdown` was stopped by its supervisor,
  # which happens when the VM is shutting down. Restarting it would call
  # into a supervisor that is already gone and crash the relay with an
  # EXIT trace, so just drop the reference.
  @impl GenServer
  def handle_info(
        {:DOWN, ref, :process, pid, reason},
        %{federation_ref: ref, federation: pid} = state
      )
      when is_pid(pid) do
    {:stop, {:federation_stopped, reason}, state}
  end

  def handle_info({:federation_peer_up, peer}, state) do
    if peer in state.federation_peers do
      state = %{
        state
        | catalog_ready: MapSet.put(state.catalog_ready, peer),
          catalog_queue: MapSet.put(state.catalog_queue, peer)
      }

      {:noreply, start_catalog_syncs(state)}
    else
      {:noreply, state}
    end
  end

  def handle_info(:sync_catalogs, state) do
    Process.send_after(self(), :sync_catalogs, @catalog_interval)
    state = state |> prune_directory() |> refresh_catalog()
    state = %{state | catalog_queue: MapSet.union(state.catalog_queue, state.catalog_ready)}
    {:noreply, start_catalog_syncs(state)}
  end

  def handle_info({:catalog_result, peer, token, result}, state) do
    case Map.get(state.catalog_syncs, peer) do
      %{token: ^token} = job ->
        Process.demonitor(job.monitor, [:flush])
        state = %{state | catalog_syncs: Map.delete(state.catalog_syncs, peer)}

        {catalog, more?} =
          case result do
            {:ok, reply} ->
              case FederationCatalog.apply_reply(
                     state.federation_catalog,
                     peer,
                     job.request,
                     reply
                   ) do
                {:ok, catalog, more?} -> {catalog, more?}
                {:error, catalog, _} -> {catalog, false}
              end

            _ ->
              {FederationCatalog.fail_peer(state.federation_catalog, peer), false}
          end

        state = %{
          state
          | federation_catalog: catalog,
            catalog_sync_pages: state.catalog_sync_pages + 1
        }

        state =
          if more?,
            do: %{state | catalog_queue: MapSet.put(state.catalog_queue, peer)},
            else: state

        {:noreply, state |> refresh_catalog() |> start_catalog_syncs()}

      _ ->
        {:noreply, state}
    end
  end

  def handle_info({:federation_peer_down, peer}, state) do
    {:noreply, remove_federation_peer(state, peer)}
  end

  def handle_info(:sweep_federation, state) do
    Process.send_after(self(), :sweep_federation, 30_000)
    {:noreply, prune_federation(state)}
  end

  def handle_info({:federation_result, ref, responses}, state) do
    case Map.pop(state.federation_queries, ref) do
      {nil, _} ->
        {:noreply, state}

      {query, remaining} ->
        Process.demonitor(query.monitor, [:flush])
        state = %{state | federation_queries: remaining}
        {:noreply, finish_federation_query(state, query, responses)}
    end
  end

  def handle_info({:DOWN, ref, :process, pid, reason}, state)
      when reason == :shutdown or (is_tuple(reason) and elem(reason, 0) == :shutdown) do
    if Map.get(state.acceptor_refs, pid) == ref do
      {:noreply, %{state | acceptor_refs: Map.delete(state.acceptor_refs, pid)}}
    else
      {:noreply, remove_failed_work(state, ref)}
    end
  end

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
          state = state |> remove_directory_connection(pid, ref) |> refresh_catalog()
          {:noreply, remove_failed_work(state, ref)}
      end
    end
  end

  # The acceptors cannot recover from a closed listen socket. A :normal reason would stop
  # the relay without a log entry.
  def handle_info({:EXIT, port, reason}, %{listen_socket: port} = state),
    do: {:stop, {:listen_socket_closed, reason}, state}

  def handle_info(_msg, state), do: {:noreply, state}

  @impl GenServer
  def terminate(_reason, state) do
    :gen_tcp.close(state.listen_socket)

    Enum.each(state.acceptor_refs, fn {_pid, ref} ->
      Process.demonitor(ref, [:flush])
    end)

    stop_shards(state.shard_refs)
    stop_directory_monitors(state.directory_conn_refs)

    # The federation process can stop at the same time. That must not abort the cleanup below.
    if is_pid(state.federation) do
      try do
        GenServer.stop(state.federation, :normal)
      catch
        :exit, _reason -> :ok
      end
    end

    Enum.each(state.federation_queries, fn {_ref, query} ->
      Process.exit(query.pid, :shutdown)
    end)

    Enum.each(state.catalog_syncs, fn {_peer, job} ->
      Process.exit(job.pid, :shutdown)
      Process.demonitor(job.monitor, [:flush])
    end)

    delete_tables(state.routes_tables)
    clear_runtime()

    emit([:relay, :stopped], %{count: 1}, %{})
    :ok
  end

  # --- Private ---

  defp valid_federation?(_identity, []), do: true

  defp valid_federation?(%Identity{public_key: own_key, secret_key: secret}, peers)
       when is_binary(secret) and is_list(peers) and length(peers) <= 16 do
    Enum.all?(peers, fn
      %{public_key: key, host: host, port: port} ->
        valid_pubkey?(key) and key != own_key and
          (is_binary(host) or is_list(host)) and is_integer(port) and port > 0 and port < 65_536

      _ ->
        false
    end) and MapSet.size(MapSet.new(peers, & &1.public_key)) == length(peers)
  end

  defp valid_federation?(_, _), do: false

  defp start_federation(_identity, []), do: {nil, nil}

  defp start_federation(identity, peers) do
    {:ok, pid} = Federation.start_link(relay: self(), identity: identity, peers: peers)
    Process.unlink(pid)
    {pid, Process.monitor(pid)}
  end

  defp local_entries(state) do
    state.directory_records
    |> Enum.filter(fn {pubkey, record} -> directory_live?(state, pubkey, record) end)
    |> Enum.map(fn {_key, record} -> record.entry end)
  end

  # Catalog work uses a separate bounded queue. It never recursively queries
  # other relays inside a request, and only runs on authenticated ready links.
  defp start_catalog_syncs(state) do
    available = @max_catalog_syncs - map_size(state.catalog_syncs)

    peers =
      state.catalog_queue
      |> Enum.filter(
        &(MapSet.member?(state.catalog_ready, &1) and not Map.has_key?(state.catalog_syncs, &1))
      )
      |> Enum.sort_by(&{Map.get(state.catalog_attempts, &1, 0), &1})
      |> Enum.take(available)

    Enum.reduce(peers, state, fn peer, acc ->
      request = FederationCatalog.next_request(acc.federation_catalog, peer)
      parent = self()
      manager = acc.federation
      token = make_ref()

      {:ok, pid} =
        Task.Supervisor.start_child(Arc.Net.TaskSupervisor, fn ->
          result = Federation.request(manager, peer, request, 1_250)
          send(parent, {:catalog_result, peer, token, result})
        end)

      job = %{pid: pid, monitor: Process.monitor(pid), token: token, request: request}

      %{
        acc
        | catalog_syncs: Map.put(acc.catalog_syncs, peer, job),
          catalog_queue: MapSet.delete(acc.catalog_queue, peer),
          catalog_attempts:
            Map.put(acc.catalog_attempts, peer, System.unique_integer([:monotonic, :positive]))
      }
    end)
  end

  defp cancel_catalog_sync(state, peer) do
    case Map.pop(state.catalog_syncs, peer) do
      {nil, _} ->
        state

      {job, jobs} ->
        Process.exit(job.pid, :shutdown)
        Process.demonitor(job.monitor, [:flush])
        %{state | catalog_syncs: jobs}
    end
  end

  defp fail_catalog_peer(state, peer),
    do: %{state | federation_catalog: FederationCatalog.fail_peer(state.federation_catalog, peer)}

  defp refresh_catalog(%{federation: nil} = state), do: state

  defp refresh_catalog(state) do
    catalog =
      FederationCatalog.rebuild(
        state.federation_catalog,
        local_entries(state),
        state.federation_transit
      )

    state = %{state | federation_catalog: catalog}

    if catalog.generation != state.catalog_generation do
      # Withdrawals invalidate routes learned from earlier live searches too.
      state = %{state | federation_entries: %{}, catalog_generation: catalog.generation}
      cache_imported(state, FederationCatalog.entries(catalog))
    else
      state
    end
  end

  defp cached_directory_query(state, query) do
    state = refresh_catalog(state)
    catalog = state.federation_catalog
    imported = Enum.map(FederationCatalog.entries(catalog), &elem(&1, 1))
    reply = FederationDirectory.cached(local_entries(state) ++ imported, query.request)
    search? = query.request["type"] == "search"
    warm? = FederationCatalog.synced_peers(catalog) > 0
    exact? = exact_identity_result?(query.request, reply)

    if (search? and warm?) or exact? do
      reply =
        Map.merge(reply, %{
          "ok" => true,
          "cached" => true,
          "partial" => FederationCatalog.partial?(catalog)
        })

      send_directory_reply(query.conn, query.request_id, reply)
      %{state | catalog_hits: state.catalog_hits + 1}
    else
      start_federation_query(state, query)
    end
  end

  defp handle_federation_request(state, peer, %{"type" => "catalog"} = request, _from) do
    if peer in state.federation_peers do
      state = state |> prune_directory() |> refresh_catalog()
      {reply, catalog} = FederationCatalog.request(state.federation_catalog, peer, request)
      {:reply, reply, %{state | federation_catalog: catalog}}
    else
      {:reply, %{"error" => "invalid_federation_request"}, state}
    end
  end

  defp handle_federation_request(state, peer, request, from) do
    state = prune_federation(state)

    cond do
      not valid_federation_request?(state, peer, request) ->
        {:reply, %{"error" => "invalid_federation_request"}, state}

      duplicate_network_request?(state, request) ->
        {:reply, %{"entries" => [], "routes" => %{}, "next" => :null, "partial" => true}, state}

      true ->
        state = remember_network_request(state, request)
        federation_request_reply(state, peer, request, from)
    end
  end

  defp valid_federation_request?(state, peer, request) do
    peer in state.federation_peers and
      FederationDirectory.valid_ingress?(request, peer, state.relay_public_key)
  end

  defp duplicate_network_request?(state, request) do
    FederationDirectory.network?(request) and
      (Map.has_key?(state.federation_seen, request["network"]["id"]) or
         map_size(state.federation_seen) >= @max_directory_records)
  end

  defp remember_network_request(state, request) do
    if FederationDirectory.network?(request) do
      expires = System.monotonic_time(:millisecond) + 12_000

      %{
        state
        | federation_seen: Map.put(state.federation_seen, request["network"]["id"], expires)
      }
    else
      state
    end
  end

  defp federation_request_reply(state, peer, request, from) do
    onward = state.federation_transit and FederationDirectory.network?(request)
    peers = FederationDirectory.eligible_peers(state.federation_peers, request)

    if onward and peers != [] and FederationDirectory.can_continue?(request) do
      query = %{
        kind: :peer,
        peer: peer,
        from: from,
        incoming: request,
        request: FederationDirectory.continue(request, state.relay_public_key)
      }

      {:noreply, start_federation_query(state, query)}
    else
      reply = FederationDirectory.export(local_entries(state), state.relay_public_key, request)
      partial = onward and peers != [] and not FederationDirectory.can_continue?(request)
      {:reply, Map.put(reply, "partial", partial), state}
    end
  end

  defp start_federation_query(state, query) do
    query =
      if query.kind != :peer and not FederationDirectory.network?(query.request),
        do: %{
          query
          | request: FederationDirectory.originate(query.request, state.relay_public_key)
        },
        else: query

    cond do
      not FederationDirectory.valid_request?(query.request) ->
        fail_federation_query(query, "invalid_query")
        state

      map_size(state.federation_queries) >= @max_federation_queries ->
        fail_federation_query(query, "federation_busy")
        state

      true ->
        parent = self()
        ref = make_ref()
        manager = state.federation
        peers = state.federation_peers

        {:ok, pid} =
          Task.Supervisor.start_child(Arc.Net.TaskSupervisor, fn ->
            responses = FederationDirectory.query(manager, peers, query.request)
            send(parent, {:federation_result, ref, responses})
          end)

        caller_monitor = if query.kind == :peer, do: Process.monitor(elem(query.from, 0))

        query =
          Map.merge(query, %{
            pid: pid,
            monitor: Process.monitor(pid),
            caller_monitor: caller_monitor,
            catalog_generation: state.catalog_generation
          })

        %{
          state
          | federation_queries: Map.put(state.federation_queries, ref, query),
            federation_live_queries: state.federation_live_queries + 1
        }
    end
  end

  defp finish_federation_query(state, query, responses) do
    if query.caller_monitor, do: Process.demonitor(query.caller_monitor, [:flush])

    {reply, imported} = combine_federation_query(state, query, responses)
    {state, changed?} = refresh_query_catalog(state, query, imported)
    reply = current_query_reply(state, query, reply, changed?)

    case query do
      %{kind: :peer, from: from} ->
        GenServer.reply(from, reply)
        state

      %{kind: :directory, conn: conn, pubkey: key, request_id: id} ->
        if Map.get(state.directory_clients, key) == conn and Process.alive?(conn) do
          if query.request["type"] == "resolve" and reply["partial"] and
               not exact_identity_result?(query.request, reply) do
            send_directory_error(conn, id, "federation_unavailable")
          else
            send_directory_reply(conn, id, Map.put(reply, "ok", true))
          end
        end

        state

      %{kind: :route, conn: conn, packet: packet} ->
        # Recheck the original connection after the asynchronous lookup. A
        # replaced citizen connection must not retain authority to send.
        if reply["partial"] and not exact_identity_result?(query.request, reply),
          do: state,
          else: route_remote(state, conn, packet, false)
    end
  end

  defp combine_federation_query(state, query, responses) do
    local = federation_query_local_entries(state, query)
    responses = invalidate_query_peers(responses, query)

    FederationDirectory.combine(
      local,
      responses,
      query.request,
      federation_query_options(state, query)
    )
  end

  defp federation_query_local_entries(state, %{kind: :peer, incoming: incoming}) do
    FederationDirectory.exportable(local_entries(state), state.relay_public_key, incoming)
  end

  defp federation_query_local_entries(state, _query), do: local_entries(state)

  defp federation_query_options(state, %{kind: :peer}), do: [home: state.relay_public_key]
  defp federation_query_options(_state, _query), do: []

  defp invalidate_query_peers(responses, query) do
    Enum.map(responses, fn {peer, result} ->
      if peer in Map.get(query, :invalid_peers, []),
        do: {peer, {:error, :federation_peer_unavailable}},
        else: {peer, result}
    end)
  end

  defp refresh_query_catalog(state, query, imported) do
    state = refresh_catalog(state)
    changed? = query.catalog_generation != state.catalog_generation
    {if(changed?, do: state, else: cache_imported(state, imported)), changed?}
  end

  # A search that started cold may finish after synchronization or withdrawal.
  # Use the known current catalog instead of returning an older branch result.
  defp current_query_reply(state, query, reply, true) do
    if query.kind == :directory and query.request["type"] == "search" and
         FederationCatalog.synced_peers(state.federation_catalog) > 0 do
      entries =
        local_entries(state) ++
          Enum.map(FederationCatalog.entries(state.federation_catalog), &elem(&1, 1))

      FederationDirectory.cached(entries, query.request)
      |> Map.merge(%{
        "cached" => true,
        "partial" => FederationCatalog.partial?(state.federation_catalog)
      })
    else
      Map.put(reply, "cached", false)
    end
  end

  defp current_query_reply(_state, _query, reply, false), do: Map.put(reply, "cached", false)

  # A complete public key selects one cryptographic identity even when other
  # branches are unavailable. Names and prefixes still require complete replies.
  defp exact_identity_result?(%{"type" => "resolve", "query" => query}, %{"entries" => [record]})
       when is_binary(query) and byte_size(query) == 64 do
    String.downcase(query) == record["public_key"]
  end

  defp exact_identity_result?(_, _), do: false

  defp fail_federation_query(%{kind: :directory, conn: conn, request_id: id}, reason),
    do: send_directory_error(conn, id, reason)

  defp fail_federation_query(%{kind: :peer, from: from}, reason),
    do: GenServer.reply(from, %{"error" => reason})

  defp fail_federation_query(_, _), do: :ok

  defp remove_failed_work(state, monitor) do
    state =
      Enum.reduce(state.catalog_syncs, state, fn {peer, job}, acc ->
        if job.monitor == monitor do
          acc |> cancel_catalog_sync(peer) |> fail_catalog_peer(peer) |> refresh_catalog()
        else
          acc
        end
      end)

    remove_failed_query(state, monitor)
  end

  defp remove_failed_query(state, monitor) do
    queries =
      Enum.reject(state.federation_queries, fn {_ref, query} ->
        if query.monitor == monitor or query.caller_monitor == monitor do
          fail_federation_query(query, "federation_unavailable")
          Process.exit(query.pid, :shutdown)
          Process.demonitor(query.monitor, [:flush])
          if query.caller_monitor, do: Process.demonitor(query.caller_monitor, [:flush])
          true
        else
          false
        end
      end)

    %{state | federation_queries: Map.new(queries)}
  end

  defp cache_imported(state, imported) do
    state = prune_federation(state)

    entries =
      Enum.reduce(imported, state.federation_entries, fn {peer, entry}, acc ->
        if map_size(acc) < @max_directory_records or Map.has_key?(acc, entry.public_key) do
          previous = Map.get(acc, entry.public_key)

          if previous == nil or entry.issued_at > previous.entry.issued_at or
               (entry.issued_at == previous.entry.issued_at and
                  {length(entry.relay_path), entry.relay_path} <=
                    {length(previous.entry.relay_path), previous.entry.relay_path}) do
            Map.put(acc, entry.public_key, %{
              peer: peer,
              entry: entry,
              expires_at: entry.expires_at
            })
          else
            acc
          end
        else
          acc
        end
      end)

    %{state | federation_entries: entries}
  end

  defp route_remote(state, conn, packet, resolve? \\ true) do
    state = prune_federation(state)

    with true <- state.federation != nil,
         {:ok, decoded} <- Packet.decode(packet),
         true <- valid_sender_fast?(conn, decoded.src),
         true <- fresh_packet?(decoded) do
      route_remote_packet(state, conn, packet, decoded, resolve?)
    else
      _ ->
        drop_fast(packet, :no_route)
        state
    end
  end

  defp route_remote_packet(state, conn, packet, decoded, resolve?) do
    context = remote_route_context(state, decoded)

    cond do
      destination_network_reply?(context, conn) ->
        send_network_reply(state, packet, decoded, context.network_reply)

      network_entry?(context.imported) ->
        send_network_request(state, conn, packet, decoded, context.imported.entry)

      context.peer != nil ->
        forward_to_federation_peer(state, packet, context.key, context.peer)

      resolve? ->
        start_remote_route_query(state, conn, packet, decoded.dst)

      true ->
        drop_fast(packet, :no_route)
        state
    end
  end

  defp remote_route_context(state, decoded) do
    key = {decoded.src, decoded.dst, decoded.session_id}
    imported = Map.get(state.federation_entries, decoded.dst)
    return = Map.get(state.federation_returns, key)

    %{
      key: key,
      imported: imported,
      peer: remote_route_peer(return, imported),
      network_reply:
        Map.get(state.federation_network_returns, {decoded.dst, decoded.src, decoded.session_id})
    }
  end

  defp remote_route_peer(%{kind: :inbound, peer: peer}, _imported), do: peer
  defp remote_route_peer(_return, %{peer: peer}), do: peer
  defp remote_route_peer(_return, _imported), do: nil

  defp destination_network_reply?(%{network_reply: reply}, conn) do
    reply != nil and reply.local_end == :destination and reply.local_conn == conn
  end

  defp network_entry?(%{entry: %{federation: :network}}), do: true
  defp network_entry?(_entry), do: false

  defp forward_to_federation_peer(state, packet, key, peer) do
    state = put_return(state, key, peer, :outbound)

    if match?(%{peer: ^peer}, Map.get(state.federation_returns, key)) do
      case safe_federation_forward(state.federation, peer, packet) do
        :ok -> emit([:relay, :federation, :forwarded], %{count: 1, bytes: byte_size(packet)}, %{})
        {:error, _} -> drop_fast(packet, :federation_unavailable)
      end
    end

    state
  end

  defp start_remote_route_query(state, conn, packet, destination) do
    request = %{"type" => "resolve", "query" => Base.encode16(destination, case: :lower)}
    start_federation_query(state, %{kind: :route, conn: conn, packet: packet, request: request})
  end

  defp send_network_request(state, conn, packet, decoded, entry) do
    path = [state.relay_public_key | entry.relay_path]
    route = %{packet: packet, path: path, cursor: 1, mode: :request, record: entry.record}
    key = {decoded.src, decoded.dst, decoded.session_id}

    with true <- valid_network_route?(route),
         {:ok, state} <- put_network_return(state, key, path, :source, conn),
         :ok <- Federation.forward_route(state.federation, Enum.at(path, 1), route) do
      state
    else
      _ ->
        drop_fast(packet, :federation_unavailable)
        state
    end
  end

  defp send_network_reply(state, packet, decoded, conversation) do
    route = %{
      packet: packet,
      path: Enum.reverse(conversation.path),
      cursor: 1,
      mode: :reply,
      record: nil
    }

    key = {decoded.dst, decoded.src, decoded.session_id}

    case Federation.forward_route(state.federation, Enum.at(route.path, 1), route) do
      :ok ->
        refresh_network_return(state, key)

      _ ->
        drop_fast(packet, :federation_unavailable)
        state
    end
  end

  defp receive_routed(state, peer, route) do
    state = prune_federation(state)

    with true <- peer in state.federation_peers,
         true <- valid_network_route?(route),
         true <- Enum.at(route.path, route.cursor) == state.relay_public_key,
         true <- Enum.at(route.path, route.cursor - 1) == peer,
         {:ok, decoded} <- Packet.decode(route.packet),
         true <- fresh_packet?(decoded) do
      case route.mode do
        :request -> receive_network_request(state, route, decoded)
        :reply -> receive_network_reply(state, route, decoded)
      end
    else
      _ -> state
    end
  end

  defp valid_network_route?(%{
         path: path,
         cursor: cursor,
         mode: mode,
         packet: packet,
         record: record
       })
       when is_list(path) and length(path) in 2..9 and is_integer(cursor) and
              is_binary(packet) and byte_size(packet) <= 8 * 1024 * 1024 do
    cursor >= 1 and cursor < length(path) and Enum.uniq(path) == path and
      Enum.all?(path, &valid_pubkey?/1) and
      ((mode == :request and is_map(record)) or (mode == :reply and record == nil))
  end

  defp valid_network_route?(_), do: false

  defp receive_network_request(state, route, decoded) do
    with {:ok, %{federation: :network} = entry} <- RelayAnnouncement.verify(route.record),
         true <- entry.public_key == decoded.dst,
         true <- entry.relay_public_key == List.last(route.path) do
      key = {decoded.src, decoded.dst, decoded.session_id}

      if route.cursor == length(route.path) - 1 do
        with true <- network_destination?(state, decoded.dst),
             conn when is_pid(conn) <- lookup_route(decoded.dst),
             true <- Process.alive?(conn),
             {:ok, state} <- put_network_return(state, key, route.path, :destination, conn) do
          forward_fast(conn, route.packet)
          state
        else
          _ ->
            drop_fast(route.packet, :federation_destination_not_shared)
            state
        end
      else
        next = Enum.at(route.path, route.cursor + 1)

        with true <- state.federation_transit and next in state.federation_peers,
             {:ok, state} <- put_network_return(state, key, route.path, :transit, nil),
             :ok <-
               Federation.forward_route(state.federation, next, %{
                 route
                 | cursor: route.cursor + 1
               }) do
          state
        else
          _ ->
            drop_fast(route.packet, :federation_transit_denied)
            state
        end
      end
    else
      _ ->
        drop_fast(route.packet, :invalid_federation_announcement)
        state
    end
  end

  defp receive_network_reply(state, route, decoded) do
    key = {decoded.dst, decoded.src, decoded.session_id}

    with %{path: path} = conversation <- Map.get(state.federation_network_returns, key),
         true <- Enum.reverse(path) == route.path do
      if route.cursor == length(route.path) - 1 do
        if conversation.local_end == :source and
             valid_sender_fast?(conversation.local_conn, decoded.dst) do
          forward_fast(conversation.local_conn, route.packet)
          refresh_network_return(state, key)
        else
          state
        end
      else
        next = Enum.at(route.path, route.cursor + 1)

        with true <- conversation.local_end == :transit and state.federation_transit,
             true <- next in state.federation_peers,
             :ok <-
               Federation.forward_route(state.federation, next, %{
                 route
                 | cursor: route.cursor + 1
               }) do
          refresh_network_return(state, key)
        else
          _ -> state
        end
      end
    else
      _ ->
        drop_fast(route.packet, :federation_no_return_permission)
        state
    end
  end

  defp network_destination?(state, key) do
    case Map.get(state.directory_records, key) do
      %{entry: %{federation: :network}} = record -> directory_live?(state, key, record)
      _ -> false
    end
  end

  defp put_network_return(state, key, path, local_end, conn) do
    old = Map.get(state.federation_network_returns, key)

    cond do
      old != nil and (old.path != path or old.local_end != local_end or old.local_conn != conn) ->
        :error

      old == nil and map_size(state.federation_network_returns) >= @max_directory_records ->
        :error

      true ->
        route = %{
          path: path,
          local_end: local_end,
          local_conn: conn,
          expires_at: System.system_time(:second) + @return_route_ttl
        }

        {:ok,
         %{
           state
           | federation_network_returns: Map.put(state.federation_network_returns, key, route)
         }}
    end
  end

  defp refresh_network_return(state, key) do
    routes =
      Map.update!(
        state.federation_network_returns,
        key,
        &%{&1 | expires_at: System.system_time(:second) + @return_route_ttl}
      )

    %{state | federation_network_returns: routes}
  end

  defp receive_federated(state, peer, packet) do
    state = prune_federation(state)

    with true <- peer in state.federation_peers,
         {:ok, decoded} <- Packet.decode(packet),
         true <- fresh_packet?(decoded),
         conn when is_pid(conn) <- lookup_route(decoded.dst),
         true <- Process.alive?(conn) do
      key = {decoded.dst, decoded.src, decoded.session_id}
      return = Map.get(state.federation_returns, key)
      expected_reply? = return != nil and return.kind == :outbound and return.peer == peer

      if expected_reply? or exported_destination?(state, decoded.dst) do
        state = put_return(state, key, peer, :inbound)

        if match?(%{peer: ^peer}, Map.get(state.federation_returns, key)) do
          forward_fast(conn, packet)
        end

        state
      else
        drop_fast(packet, :federation_destination_not_shared)
        state
      end
    else
      _ ->
        # A peer-originated packet is never forwarded to another peer.
        drop_fast(packet, :federation_no_local_route)
        state
    end
  end

  defp exported_destination?(state, key) do
    case Map.get(state.directory_records, key) do
      nil ->
        false

      record ->
        directory_live?(state, key, record) and
          RelayAnnouncement.federatable?(record.entry, state.relay_public_key)
    end
  end

  defp fresh_packet?(decoded) do
    abs(System.system_time(:millisecond) - decoded.ts) <= 120_000 and
      byte_size(decoded.src) == 32 and byte_size(decoded.dst) == 32 and
      byte_size(decoded.session_id) == 16
  end

  defp put_return(state, key, peer, kind) do
    expires = System.system_time(:second) + @return_route_ttl
    returns = state.federation_returns

    cond do
      Map.has_key?(returns, key) ->
        old = Map.fetch!(returns, key)

        if old.peer == peer,
          do: %{state | federation_returns: Map.put(returns, key, %{old | expires_at: expires})},
          else: state

      map_size(returns) < @max_directory_records ->
        %{
          state
          | federation_returns:
              Map.put(returns, key, %{peer: peer, kind: kind, expires_at: expires})
        }

      true ->
        state
    end
  end

  defp remove_federation_peer(state, peer) do
    state = state |> cancel_catalog_sync(peer) |> fail_catalog_peer(peer) |> refresh_catalog()

    state = %{
      state
      | catalog_ready: MapSet.delete(state.catalog_ready, peer),
        catalog_queue: MapSet.delete(state.catalog_queue, peer)
    }

    # A disappearing callback/link must not leave recursive searches alive or
    # let their late responses reinstall routes through a disconnected peer.
    queries =
      Enum.reduce(state.federation_queries, %{}, fn {ref, query}, acc ->
        if Map.get(query, :peer) == peer do
          fail_federation_query(query, "federation_unavailable")
          Process.exit(query.pid, :shutdown)
          Process.demonitor(query.monitor, [:flush])
          if query.caller_monitor, do: Process.demonitor(query.caller_monitor, [:flush])
          acc
        else
          query = Map.update(query, :invalid_peers, [peer], &[peer | &1])
          Map.put(acc, ref, query)
        end
      end)

    %{
      state
      | federation_queries: queries,
        federation_entries:
          Map.reject(state.federation_entries, fn {_, route} -> route.peer == peer end),
        federation_returns:
          Map.reject(state.federation_returns, fn {_, route} -> route.peer == peer end),
        federation_network_returns:
          Map.reject(state.federation_network_returns, fn {_, route} -> peer in route.path end)
    }
  end

  defp prune_federation(state) do
    now = System.system_time(:second)
    monotonic_now = System.monotonic_time(:millisecond)

    %{
      state
      | federation_entries:
          Map.reject(state.federation_entries, fn {_, route} -> route.expires_at <= now end),
        federation_returns:
          Map.reject(state.federation_returns, fn {{local, _, _}, route} ->
            route.expires_at <= now or not is_pid(lookup_route(local))
          end),
        federation_network_returns:
          Map.reject(state.federation_network_returns, fn {{source, dest, _}, route} ->
            route.expires_at <= now or
              (route.local_end == :source and lookup_route(source) != route.local_conn) or
              (route.local_end == :destination and lookup_route(dest) != route.local_conn)
          end),
        federation_seen:
          Map.reject(state.federation_seen, fn {_, expires} -> expires <= monotonic_now end)
    }
  end

  defp safe_federation_forward(manager, peer, packet) do
    Federation.forward(manager, peer, packet)
  catch
    :exit, _ -> {:error, :federation_unavailable}
  end

  defp federation_queue_available?(manager) do
    case Process.info(manager, :message_queue_len) do
      {:message_queue_len, count} -> count < 32
      _ -> false
    end
  end

  defp route_packet_fast(relay_pid, conn_pid, packet) do
    case Packet.decode(packet) do
      {:ok, %{src: src_pk, dst: dst_pk}} ->
        if valid_sender_fast?(conn_pid, src_pk) do
          case lookup_route(dst_pk) do
            nil -> GenServer.cast(relay_pid, {:route_remote, conn_pid, packet})
            destination -> forward_fast(destination, packet)
          end
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
        nil ->
          {:noreply, state}

        shard_pid ->
          # A dead shard loses the cast. Its :DOWN can still be in the mailbox, so restart
          # the shard now.
          {shard_pid, state} = live_shard(state, shard_pid, pubkey)
          RouteShard.register(shard_pid, conn_pid, pubkey)
          {:noreply, state}
      end
    else
      {:noreply, state}
    end
  end

  defp live_shard(state, shard_pid, pubkey) do
    if Process.alive?(shard_pid) do
      {shard_pid, state}
    else
      idx = :erlang.phash2(pubkey, state.route_partitions)
      state = restart_shard(state, shard_pid, idx)
      {Map.fetch!(state.shard_pids, idx), state}
    end
  end

  # Directory control is deliberately handled in the relay process. It is
  # bounded, infrequent metadata; packet forwarding remains on the ETS path.
  defp handle_directory_control(state, conn_pid, pubkey, payload) do
    case decode_directory_request(payload) do
      {:ok, request_id, request} ->
        handle_directory_request(state, conn_pid, pubkey, request_id, request)

      :error ->
        state
    end
  end

  defp handle_directory_request(state, conn_pid, pubkey, request_id, %{
         "type" => "announce",
         "record" => record
       }) do
    announce_directory_record(state, conn_pid, pubkey, request_id, record)
  end

  defp handle_directory_request(state, conn_pid, pubkey, request_id, %{
         "type" => "resolve",
         "query" => query
       }) do
    resolve_directory_request(state, conn_pid, pubkey, request_id, query)
  end

  defp handle_directory_request(
         state,
         conn_pid,
         pubkey,
         request_id,
         %{"type" => "search", "query" => query} = request
       ) do
    search_directory_request(state, conn_pid, pubkey, request_id, query, request)
  end

  defp handle_directory_request(
         state,
         conn_pid,
         _pubkey,
         request_id,
         %{"type" => "observe"} = request
       ) do
    observe_directory_endpoint(state, conn_pid, request_id, request)
  end

  defp handle_directory_request(
         state,
         conn_pid,
         _pubkey,
         request_id,
         %{"type" => "status"} = request
       ) do
    relay_status_request(state, conn_pid, request_id, request)
  end

  defp handle_directory_request(state, conn_pid, _pubkey, request_id, _request) do
    send_directory_error(conn_pid, request_id, "invalid_request")
    state
  end

  defp observe_directory_endpoint(state, conn_pid, request_id, request) do
    if self_observation_request?(request) do
      case Connection.peer_endpoint(conn_pid) do
        {:ok, endpoint} ->
          send_directory_reply(conn_pid, request_id, %{
            "ok" => true,
            "observed" => observed_endpoint(endpoint)
          })

        {:error, _reason} ->
          send_directory_error(conn_pid, request_id, "observation_unavailable")
      end
    else
      send_directory_error(conn_pid, request_id, "invalid_request")
    end

    state
  end

  defp relay_status_request(state, conn_pid, request_id, request) do
    if self_observation_request?(request) do
      send_directory_reply(conn_pid, request_id, %{
        "ok" => true,
        "status" => %{
          "role" => "relay",
          "state" => "running",
          "version" => relay_version(),
          "public_key" => Base.encode16(state.relay_public_key, case: :lower),
          "uptime_seconds" => relay_uptime_seconds(state),
          "federation_transit" => state.federation_transit
        }
      })
    else
      send_directory_error(conn_pid, request_id, "invalid_request")
    end

    state
  end

  defp relay_uptime_seconds(state) do
    max(System.monotonic_time(:second) - state.started_at, 0)
  end

  defp relay_version do
    case Application.spec(:arc_net, :vsn) do
      version when is_list(version) -> List.to_string(version)
      version when is_binary(version) -> version
      _ -> "unknown"
    end
  end

  defp self_observation_request?(request),
    do: Enum.sort(Map.keys(request)) == ["request_id", "type"]

  defp observed_endpoint({ip, port}) when is_integer(port) and port in 1..65_535 do
    %{"host" => ip |> :inet.ntoa() |> List.to_string(), "port" => port}
  end

  defp announce_directory_record(state, conn_pid, pubkey, request_id, record) do
    with {:ok, entry} <- verify_announcement(record, pubkey, state.relay_public_key),
         {:ok, state} <- put_directory_record(state, pubkey, conn_pid, entry) do
      send_directory_reply(conn_pid, request_id, %{"ok" => true})
      state
    else
      {:error, state} ->
        send_directory_error(conn_pid, request_id, "directory_full")
        state

      :error ->
        send_directory_error(conn_pid, request_id, "invalid_announcement")
        state
    end
  end

  defp resolve_directory_request(state, conn_pid, pubkey, request_id, query) do
    cond do
      not valid_directory_query?(query) ->
        send_directory_error(conn_pid, request_id, "invalid_query")
        state

      state.federation != nil ->
        cached_directory_query(
          state,
          directory_query(conn_pid, pubkey, request_id, %{"type" => "resolve", "query" => query})
        )

      true ->
        entries = directory_entries(state, query, :resolve) |> Enum.take(2)
        send_directory_reply(conn_pid, request_id, %{"ok" => true, "entries" => entries})
        state
    end
  end

  defp search_directory_request(state, conn_pid, pubkey, request_id, query, request) do
    with true <- valid_directory_query?(query),
         {:ok, after_cursor} <- validate_directory_cursor(Map.get(request, "after")) do
      limit = directory_limit(Map.get(request, "limit"))
      search_directory_entries(state, conn_pid, pubkey, request_id, query, after_cursor, limit)
    else
      _ ->
        send_directory_error(conn_pid, request_id, "invalid_query")
        state
    end
  end

  defp search_directory_entries(state, conn_pid, pubkey, request_id, query, after_cursor, limit) do
    if state.federation do
      request = %{"type" => "search", "query" => query, "limit" => limit}
      request = if after_cursor, do: Map.put(request, "after", after_cursor), else: request
      cached_directory_query(state, directory_query(conn_pid, pubkey, request_id, request))
    else
      {entries, next, total} =
        paged_directory_entries(state, query, after_cursor, limit, request_id)

      send_directory_reply(conn_pid, request_id, %{
        "ok" => true,
        "entries" => entries,
        "next" => next || :null,
        "total" => total
      })

      state
    end
  end

  defp directory_query(conn_pid, pubkey, request_id, request) do
    %{kind: :directory, conn: conn_pid, pubkey: pubkey, request_id: request_id, request: request}
  end

  defp decode_directory_request(payload) do
    case :json.decode(payload) do
      %{"request_id" => request_id} = request
      when is_binary(request_id) and byte_size(request_id) == 32 ->
        if valid_request_id?(request_id), do: {:ok, request_id, request}, else: :error

      _ ->
        :error
    end
  rescue
    _ -> :error
  end

  defp verify_announcement(record, pubkey, relay_key) do
    now = System.system_time(:second)

    case Arc.Data.RelayAnnouncement.verify(record, now: now) do
      {:ok, %{public_key: ^pubkey} = entry} ->
        if entry.federation == :local or entry.relay_public_key == relay_key,
          do: {:ok, entry},
          else: :error

      _ ->
        :error
    end
  rescue
    _ -> :error
  end

  defp put_directory_record(state, pubkey, conn_pid, entry) do
    records = Map.delete(state.directory_records, pubkey)

    if map_size(records) >= @max_directory_records do
      # A currently connected publisher may replace its own listing, but a full
      # directory never evicts another citizen's active offer.
      {:error, state}
    else
      record = %{
        entry: entry,
        conn_pid: conn_pid,
        expires_at: min(entry.expires_at, System.system_time(:second) + @directory_ttl_seconds),
        cursor: Base.encode16(pubkey, case: :lower)
      }

      {:ok, %{state | directory_records: Map.put(records, pubkey, record)}}
    end
  end

  defp directory_entries(state, query, mode) do
    state.directory_records
    |> Enum.filter(fn {pubkey, record} ->
      directory_live?(state, pubkey, record) and directory_matches?(record.entry, query, mode)
    end)
    |> Enum.sort_by(fn {_pubkey, record} -> record.cursor end)
    |> Enum.map(fn {_pubkey, record} -> directory_record(record.entry) end)
  end

  defp paged_directory_entries(state, query, after_cursor, limit, request_id) do
    matching =
      state.directory_records
      |> Enum.filter(fn {pubkey, record} ->
        directory_live?(state, pubkey, record) and
          directory_matches?(record.entry, query, :search)
      end)
      |> Enum.sort_by(fn {_pubkey, record} -> record.cursor end)

    total = length(matching)

    remaining = maybe_after(matching, after_cursor)
    page = remaining |> Enum.take(limit) |> fit_directory_page(request_id, total)

    next =
      if length(remaining) > length(page) and page != [] do
        page |> List.last() |> elem(1) |> Map.fetch!(:cursor)
      else
        nil
      end

    {Enum.map(page, fn {_pubkey, record} -> directory_record(record.entry) end), next, total}
  end

  defp maybe_after(entries, after_cursor) when is_binary(after_cursor),
    do: Enum.drop_while(entries, fn {_pubkey, record} -> record.cursor <= after_cursor end)

  defp maybe_after(entries, _), do: entries

  defp directory_matches?(entry, query, :resolve) do
    Arc.Data.RelayAnnouncement.matches?(entry, query)
  rescue
    _ -> false
  end

  defp directory_matches?(entry, query, :search) do
    entry.capabilities != [] and Arc.Data.RelayAnnouncement.search_match?(entry, query)
  rescue
    _ -> false
  end

  defp directory_record(entry), do: Map.get(entry, :record)

  defp directory_live?(state, pubkey, record) do
    record.expires_at > System.system_time(:second) and
      Map.get(state.directory_clients, pubkey) == record.conn_pid and
      Process.alive?(record.conn_pid)
  end

  defp prune_directory(state) do
    now = System.system_time(:second)

    records =
      Enum.reduce(state.directory_records, %{}, fn {pubkey, record}, acc ->
        if record.expires_at > now and directory_live?(state, pubkey, record),
          do: Map.put(acc, pubkey, record),
          else: acc
      end)

    %{state | directory_records: records}
  end

  defp register_directory_client(state, conn_pid, pubkey) do
    state = remove_directory_client_for_pubkey(state, pubkey)
    ref = Process.monitor(conn_pid)

    %{
      state
      | directory_clients: Map.put(state.directory_clients, pubkey, conn_pid),
        directory_conn_refs:
          Map.put(state.directory_conn_refs, conn_pid, %{pubkey: pubkey, ref: ref})
    }
  end

  defp remove_directory_client_for_pubkey(state, pubkey) do
    case Map.get(state.directory_clients, pubkey) do
      nil ->
        state

      conn_pid ->
        refs =
          case Map.pop(state.directory_conn_refs, conn_pid) do
            {%{ref: ref}, remaining} ->
              Process.demonitor(ref, [:flush])
              remaining

            _ ->
              state.directory_conn_refs
          end

        %{
          state
          | directory_clients: Map.delete(state.directory_clients, pubkey),
            directory_conn_refs: refs,
            directory_records: Map.delete(state.directory_records, pubkey)
        }
    end
  end

  defp remove_directory_connection(state, conn_pid, ref) do
    case Map.get(state.directory_conn_refs, conn_pid) do
      %{pubkey: pubkey, ref: ^ref} ->
        %{
          state
          | directory_conn_refs: Map.delete(state.directory_conn_refs, conn_pid),
            directory_clients: Map.delete(state.directory_clients, pubkey),
            directory_records: Map.delete(state.directory_records, pubkey)
        }

      _ ->
        state
    end
  end

  defp stop_directory_monitors(refs) do
    Enum.each(refs, fn {_pid, %{ref: ref}} -> Process.demonitor(ref, [:flush]) end)
  end

  defp send_directory_reply(conn_pid, request_id, fields) do
    try do
      Connection.send_control(
        conn_pid,
        Map.merge(%{"type" => "reply", "request_id" => request_id}, fields)
      )
    catch
      :exit, _ -> :ok
    end

    :ok
  end

  defp send_directory_error(conn_pid, request_id, reason),
    do: send_directory_reply(conn_pid, request_id, %{"ok" => false, "error" => reason})

  defp directory_limit(value) when is_integer(value) and value > 0,
    do: min(value, @max_directory_limit)

  defp directory_limit(_), do: 10

  defp valid_directory_query?(query), do: is_binary(query) and byte_size(query) <= 256

  defp validate_directory_cursor(nil), do: {:ok, nil}
  defp validate_directory_cursor(:null), do: {:ok, nil}

  defp validate_directory_cursor(cursor) when is_binary(cursor) do
    if Regex.match?(~r/\A[0-9a-f]{64}\z/, cursor), do: {:ok, cursor}, else: :error
  end

  defp validate_directory_cursor(_), do: :error

  defp fit_directory_page(page, request_id, total) do
    records = Enum.map(page, fn {_pubkey, record} -> directory_record(record.entry) end)

    reply = %{
      "type" => "reply",
      "request_id" => request_id,
      "ok" => true,
      "entries" => records,
      "next" => :null,
      "total" => total
    }

    if byte_size(IO.iodata_to_binary(:json.encode(reply))) <= @max_directory_reply_bytes or
         page == [] do
      page
    else
      fit_directory_page(Enum.drop(page, -1), request_id, total)
    end
  end

  defp valid_request_id?(id), do: Regex.match?(~r/\A[0-9a-f]{32}\z/, id)

  defp shard_pid_from_state(state, pubkey) do
    idx = :erlang.phash2(pubkey, state.route_partitions)
    Map.get(state.shard_pids, idx)
  end

  # Connections register only once, at hello. The new shard keeps the routes in the table and
  # takes them over in RouteShard.init/1.
  defp restart_shard(state, dead_pid, idx) do
    case Map.get(state.shard_refs, dead_pid) do
      %{ref: ref} -> Process.demonitor(ref, [:flush])
      _ -> :ok
    end

    {new_pid, new_ref} = start_shard(idx, Enum.at(state.routes_tables, idx))

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
      Task.Supervisor.start_child(
        Arc.Net.TaskSupervisor,
        Acceptor,
        :accept_loop,
        [listen_socket, relay_pid, relay_public_key]
      )

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
      %{routes_tables: routes_tables} = runtime ->
        clear_runtime()
        delete_tables(routes_tables)
        # A previous relay that skipped terminate/2 can leave its shards running.
        runtime
        |> Map.get(:shard_pids, {})
        |> Tuple.to_list()
        |> Enum.each(&Process.exit(&1, :kill))

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
