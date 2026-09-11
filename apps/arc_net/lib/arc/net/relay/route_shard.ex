defmodule Arc.Net.Relay.RouteShard do
  @moduledoc false

  use GenServer

  alias Arc.Net.Connection
  alias Arc.Net.Telemetry

  @pubkey_bytes 32

  def start_link(opts) when is_list(opts) do
    GenServer.start_link(__MODULE__, opts)
  end

  def register(shard_pid, conn_pid, pubkey) do
    GenServer.cast(shard_pid, {:register, conn_pid, pubkey})
  end

  def stats(shard_pid) do
    GenServer.call(shard_pid, :stats)
  end

  @impl GenServer
  def init(opts) do
    state = %{
      index: Keyword.fetch!(opts, :index),
      routes_table: Keyword.fetch!(opts, :routes_table),
      routes: %{},
      conns: %{}
    }

    {:ok, state}
  end

  @impl GenServer
  def handle_call(:stats, _from, state) do
    {:reply, %{routes: map_size(state.routes), conns: map_size(state.conns)}, state}
  end

  @impl GenServer
  def handle_cast({:register, conn_pid, pubkey}, state) do
    if is_pid(conn_pid) and valid_pubkey?(pubkey) do
      {state, old_conn} = upsert_route(state, conn_pid, pubkey)

      emit([:relay, :route, :registered], %{count: 1, routes: map_size(state.routes)}, %{
        conn_pid: conn_pid,
        shard: state.index
      })

      if old_conn do
        emit([:relay, :route, :replaced], %{count: 1, routes: map_size(state.routes)}, %{
          old_conn_pid: old_conn,
          new_conn_pid: conn_pid,
          shard: state.index
        })
      end

      close_old_conn(old_conn)
      {:noreply, state}
    else
      {:noreply, state}
    end
  end

  @impl GenServer
  def handle_info({:DOWN, ref, :process, conn_pid, _reason}, state) do
    case Map.get(state.conns, conn_pid) do
      %{ref: ^ref, pubkey: pubkey} ->
        conns = Map.delete(state.conns, conn_pid)

        routes =
          case Map.get(state.routes, pubkey) do
            ^conn_pid ->
              _ = ets_delete(state.routes_table, pubkey)

              emit(
                [:relay, :route, :unregistered],
                %{count: 1, routes: map_size(state.routes) - 1},
                %{conn_pid: conn_pid, shard: state.index}
              )

              Map.delete(state.routes, pubkey)

            _ ->
              state.routes
          end

        {:noreply, %{state | routes: routes, conns: conns}}

      _ ->
        {:noreply, state}
    end
  end

  def handle_info(_msg, state), do: {:noreply, state}

  @impl GenServer
  def terminate(_reason, state) do
    Enum.each(state.conns, fn {_pid, %{ref: ref}} ->
      Process.demonitor(ref, [:flush])
    end)

    :ok
  end

  defp upsert_route(state, conn_pid, pubkey) do
    {routes, conns, old_conn} =
      case Map.get(state.routes, pubkey) do
        nil ->
          {state.routes, state.conns, nil}

        ^conn_pid ->
          {state.routes, state.conns, nil}

        old_conn_pid ->
          {old_entry, conns} = Map.pop(state.conns, old_conn_pid)

          if old_entry do
            Process.demonitor(old_entry.ref, [:flush])
          end

          {state.routes, conns, old_conn_pid}
      end

    {routes, conns, ref} =
      case Map.get(conns, conn_pid) do
        nil ->
          ref = Process.monitor(conn_pid)
          {routes, Map.put(conns, conn_pid, %{pubkey: pubkey, ref: ref}), ref}

        %{pubkey: existing_pubkey, ref: ref} ->
          routes =
            if existing_pubkey != pubkey and Map.get(routes, existing_pubkey) == conn_pid do
              _ = ets_delete(state.routes_table, existing_pubkey)
              Map.delete(routes, existing_pubkey)
            else
              routes
            end

          {routes, Map.put(conns, conn_pid, %{pubkey: pubkey, ref: ref}), ref}
      end

    _ = ref

    routes = Map.put(routes, pubkey, conn_pid)
    _ = ets_insert(state.routes_table, {pubkey, conn_pid})

    {%{state | routes: routes, conns: conns}, old_conn}
  end

  defp close_old_conn(nil), do: :ok

  defp close_old_conn(conn_pid) do
    Connection.close(conn_pid)
  end

  defp valid_pubkey?(pubkey) when is_binary(pubkey), do: byte_size(pubkey) == @pubkey_bytes
  defp valid_pubkey?(_), do: false

  defp ets_insert(table, entry) do
    :ets.insert(table, entry)
  catch
    :error, :badarg -> false
  end

  defp ets_delete(table, key) do
    :ets.delete(table, key)
  catch
    :error, :badarg -> false
  end

  defp emit(event_suffix, measurements, metadata) do
    Telemetry.execute(event_suffix, measurements, metadata)
  end
end
