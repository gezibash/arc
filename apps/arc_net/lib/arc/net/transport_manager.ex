defmodule Arc.Net.TransportManager do
  @moduledoc """
  Identity-scoped relay transport pool with owner leases and idle cleanup.

  Each transport process manages exactly one source identity. The manager
  reuses that transport across callers, tracks non-persistent owners, and
  reaps idle leased transports after the last owner disappears.
  """

  use GenServer

  alias Arc.Data.Mailbox
  alias Arc.Identity
  alias Arc.Net.Transport

  @default_idle_timeout_ms 30_000

  @type owner_ref :: reference()
  @type transport_ref :: reference()

  @spec start_link(keyword()) :: GenServer.on_start()
  def start_link(opts \\ []) do
    GenServer.start_link(__MODULE__, opts, name: __MODULE__)
  end

  @spec connect_relay(charlist() | String.t(), pos_integer(), Identity.t(), binary() | nil) ::
          :ok | {:error, term()}
  def connect_relay(host, port, identity, relay_pubkey_pin \\ nil) do
    GenServer.call(__MODULE__, {:connect_relay, host, port, identity, relay_pubkey_pin})
  end

  @spec acquire(charlist() | String.t(), pos_integer(), Identity.t(), binary() | nil, keyword()) ::
          :ok | {:error, term()}
  def acquire(host, port, identity, relay_pubkey_pin \\ nil, opts \\ []) when is_list(opts) do
    GenServer.call(__MODULE__, {:acquire, host, port, identity, relay_pubkey_pin, opts})
  end

  @spec release(Identity.t() | binary(), pid()) :: :ok
  def release(identity_or_pubkey, owner_pid \\ self()) do
    GenServer.call(__MODULE__, {:release, identity_or_pubkey, owner_pid})
  end

  @spec deliver(binary() | nil, binary(), binary()) :: :ok
  def deliver(from_pk, to_pk, packet) do
    GenServer.cast(__MODULE__, {:deliver, from_pk, to_pk, packet})
  end

  @spec lookup(Identity.t() | binary()) :: {:ok, pid()} | :error
  def lookup(identity_or_pubkey) do
    GenServer.call(__MODULE__, {:lookup, identity_or_pubkey})
  end

  @spec count() :: non_neg_integer()
  def count do
    GenServer.call(__MODULE__, :count)
  end

  @spec reset() :: :ok
  def reset do
    GenServer.call(__MODULE__, :reset)
  end

  @impl GenServer
  def init(opts) do
    {:ok,
     %{
       entries: %{},
       owner_refs: %{},
       transport_refs: %{},
       default_idle_timeout_ms:
         Keyword.get(opts, :default_idle_timeout_ms, @default_idle_timeout_ms)
     }}
  end

  @impl GenServer
  def handle_call({:connect_relay, host, port, identity, relay_pubkey_pin}, _from, state) do
    with {:ok, pubkey} <- source_pubkey(identity),
         {:ok, entry, state, created?} <- ensure_entry(pubkey, state),
         entry <- cancel_idle_shutdown(entry),
         result <-
           Transport.connect_relay(entry.transport, host, port, identity, relay_pubkey_pin) do
      case result do
        :ok ->
          entry = %{entry | persistent?: true}
          {:reply, :ok, put_entry(state, pubkey, entry)}

        {:error, reason} ->
          state = maybe_discard_failed_entry(pubkey, entry, state, created?)
          {:reply, {:error, reason}, state}
      end
    else
      {:error, reason} ->
        {:reply, {:error, reason}, state}
    end
  end

  def handle_call({:acquire, host, port, identity, relay_pubkey_pin, opts}, _from, state) do
    owner_pid = Keyword.get(opts, :owner_pid, self())
    idle_timeout_ms = Keyword.get(opts, :idle_timeout_ms, state.default_idle_timeout_ms)

    with true <- is_pid(owner_pid) or {:error, :invalid_owner},
         {:ok, pubkey} <- source_pubkey(identity),
         {:ok, entry, state, created?} <- ensure_entry(pubkey, state),
         entry <- cancel_idle_shutdown(entry),
         result <-
           Transport.connect_relay(entry.transport, host, port, identity, relay_pubkey_pin) do
      case result do
        :ok ->
          {entry, state} = add_owner(pubkey, owner_pid, entry, idle_timeout_ms, state)
          {:reply, :ok, put_entry(state, pubkey, entry)}

        {:error, reason} ->
          state = maybe_discard_failed_entry(pubkey, entry, state, created?)
          {:reply, {:error, reason}, state}
      end
    else
      {:error, reason} ->
        {:reply, {:error, reason}, state}
    end
  end

  def handle_call({:release, identity_or_pubkey, owner_pid}, _from, state) do
    state =
      case source_pubkey(identity_or_pubkey) do
        {:ok, pubkey} -> remove_owner(pubkey, owner_pid, state)
        {:error, _reason} -> state
      end

    {:reply, :ok, state}
  end

  def handle_call({:lookup, identity_or_pubkey}, _from, state) do
    reply =
      with {:ok, pubkey} <- source_pubkey(identity_or_pubkey),
           %{transport: transport} <- Map.get(state.entries, pubkey),
           true <- Process.alive?(transport) do
        {:ok, transport}
      else
        _ -> :error
      end

    {:reply, reply, state}
  end

  def handle_call(:count, _from, state) do
    {:reply, map_size(state.entries), state}
  end

  def handle_call(:reset, _from, state) do
    state =
      Enum.reduce(Map.keys(state.entries), state, fn pubkey, acc ->
        delete_entry(pubkey, acc, stop_transport?: true)
      end)

    {:reply, :ok, state}
  end

  @impl GenServer
  def handle_cast({:deliver, nil, to_pk, packet}, state) do
    Mailbox.deliver(to_pk, packet)
    {:noreply, state}
  end

  def handle_cast({:deliver, from_pk, to_pk, packet}, state) do
    case Map.get(state.entries, from_pk) do
      %{transport: transport} = entry when is_pid(transport) ->
        if Process.alive?(transport) do
          send(transport, {:deliver, to_pk, packet})
          {:noreply, state}
        else
          Mailbox.deliver(to_pk, packet)

          {:noreply,
           delete_entry(from_pk, put_entry(state, from_pk, entry), stop_transport?: false)}
        end

      _ ->
        Mailbox.deliver(to_pk, packet)
        {:noreply, state}
    end
  end

  @impl GenServer
  def handle_info({:DOWN, ref, :process, _pid, _reason}, state) do
    cond do
      Map.has_key?(state.transport_refs, ref) ->
        {pubkey, transport_refs} = Map.pop(state.transport_refs, ref)

        state =
          state
          |> Map.put(:transport_refs, transport_refs)
          |> delete_entry(pubkey, stop_transport?: false, clear_transport_ref?: false)

        {:noreply, state}

      Map.has_key?(state.owner_refs, ref) ->
        {{pubkey, owner_pid}, owner_refs} = Map.pop(state.owner_refs, ref)
        state = remove_owner(pubkey, owner_pid, %{state | owner_refs: owner_refs}, owner_ref: ref)
        {:noreply, state}

      true ->
        {:noreply, state}
    end
  end

  def handle_info({:idle_timeout, pubkey, token}, state) do
    case Map.get(state.entries, pubkey) do
      %{idle_token: ^token, owners: owners, persistent?: false} when map_size(owners) == 0 ->
        {:noreply, delete_entry(pubkey, state, stop_transport?: true)}

      _ ->
        {:noreply, state}
    end
  end

  def handle_info(_msg, state), do: {:noreply, state}

  defp ensure_entry(pubkey, state) do
    case Map.get(state.entries, pubkey) do
      %{transport: transport} = entry when is_pid(transport) ->
        if Process.alive?(transport) do
          {:ok, entry, state, false}
        else
          if entry.transport_ref do
            Process.demonitor(entry.transport_ref, [:flush])
          end

          state = delete_entry(pubkey, state, stop_transport?: false)
          start_entry(pubkey, state)
        end

      %{transport_ref: transport_ref} ->
        if transport_ref do
          Process.demonitor(transport_ref, [:flush])
        end

        state = delete_entry(pubkey, state, stop_transport?: false)
        start_entry(pubkey, state)

      nil ->
        start_entry(pubkey, state)
    end
  end

  defp start_entry(pubkey, state) do
    case DynamicSupervisor.start_child(Arc.Net.TransportSupervisor, {Transport, []}) do
      {:ok, transport} ->
        transport_ref = Process.monitor(transport)

        entry = %{
          transport: transport,
          transport_ref: transport_ref,
          owners: %{},
          persistent?: false,
          idle_timer_ref: nil,
          idle_token: nil,
          idle_timeout_ms: state.default_idle_timeout_ms
        }

        state =
          state
          |> put_entry(pubkey, entry)
          |> Map.update!(:transport_refs, &Map.put(&1, transport_ref, pubkey))

        {:ok, entry, state, true}

      {:error, {:already_started, transport}} ->
        transport_ref = Process.monitor(transport)

        entry = %{
          transport: transport,
          transport_ref: transport_ref,
          owners: %{},
          persistent?: false,
          idle_timer_ref: nil,
          idle_token: nil,
          idle_timeout_ms: state.default_idle_timeout_ms
        }

        state =
          state
          |> put_entry(pubkey, entry)
          |> Map.update!(:transport_refs, &Map.put(&1, transport_ref, pubkey))

        {:ok, entry, state, true}

      {:error, reason} ->
        {:error, reason}
    end
  end

  defp add_owner(pubkey, owner_pid, entry, idle_timeout_ms, state) do
    case Map.fetch(entry.owners, owner_pid) do
      {:ok, _ref} ->
        {%{entry | idle_timeout_ms: idle_timeout_ms}, state}

      :error ->
        owner_ref = Process.monitor(owner_pid)
        owners = Map.put(entry.owners, owner_pid, owner_ref)
        entry = %{entry | owners: owners, idle_timeout_ms: idle_timeout_ms}
        state = Map.update!(state, :owner_refs, &Map.put(&1, owner_ref, {pubkey, owner_pid}))
        {entry, state}
    end
  end

  defp remove_owner(pubkey, owner_pid, state, opts \\ []) do
    case Map.get(state.entries, pubkey) do
      %{owners: owners} = entry ->
        case Map.pop(owners, owner_pid) do
          {nil, _owners} ->
            state

          {owner_ref, owners} ->
            if opts[:owner_ref] == nil do
              Process.demonitor(owner_ref, [:flush])
            end

            owner_refs =
              if opts[:owner_ref] == owner_ref do
                state.owner_refs
              else
                Map.delete(state.owner_refs, owner_ref)
              end

            entry = %{entry | owners: owners}
            state = %{state | owner_refs: owner_refs}

            entry =
              if map_size(owners) == 0 and not entry.persistent? do
                schedule_idle_shutdown(pubkey, entry)
              else
                entry
              end

            put_entry(state, pubkey, entry)
        end

      nil ->
        state
    end
  end

  defp schedule_idle_shutdown(pubkey, entry) do
    entry = cancel_idle_shutdown(entry)
    token = make_ref()

    timer_ref =
      Process.send_after(self(), {:idle_timeout, pubkey, token}, max(entry.idle_timeout_ms, 0))

    %{entry | idle_timer_ref: timer_ref, idle_token: token}
  end

  defp cancel_idle_shutdown(entry) do
    if entry.idle_timer_ref do
      Process.cancel_timer(entry.idle_timer_ref, async: true, info: false)
    end

    %{entry | idle_timer_ref: nil, idle_token: nil}
  end

  defp maybe_discard_failed_entry(pubkey, entry, state, created?) do
    cond do
      entry.persistent? or map_size(entry.owners) > 0 ->
        put_entry(state, pubkey, entry)

      created? ->
        delete_entry(pubkey, put_entry(state, pubkey, entry), stop_transport?: true)

      true ->
        put_entry(state, pubkey, schedule_idle_shutdown(pubkey, entry))
    end
  end

  defp delete_entry(pubkey, state, opts) do
    case Map.pop(state.entries, pubkey) do
      {nil, entries} ->
        %{state | entries: entries}

      {entry, entries} ->
        if entry.transport_ref && Keyword.get(opts, :clear_transport_ref?, true) do
          Process.demonitor(entry.transport_ref, [:flush])
        end

        Enum.each(entry.owners, fn {_owner_pid, owner_ref} ->
          Process.demonitor(owner_ref, [:flush])
        end)

        if entry.idle_timer_ref do
          Process.cancel_timer(entry.idle_timer_ref, async: true, info: false)
        end

        if Keyword.get(opts, :stop_transport?, false) and Process.alive?(entry.transport) do
          _ = DynamicSupervisor.terminate_child(Arc.Net.TransportSupervisor, entry.transport)
        end

        owner_refs =
          Enum.reduce(entry.owners, state.owner_refs, fn {_owner_pid, owner_ref}, acc ->
            Map.delete(acc, owner_ref)
          end)

        transport_refs =
          if entry.transport_ref && Keyword.get(opts, :clear_transport_ref?, true) do
            Map.delete(state.transport_refs, entry.transport_ref)
          else
            state.transport_refs
          end

        %{state | entries: entries, owner_refs: owner_refs, transport_refs: transport_refs}
    end
  end

  defp put_entry(state, pubkey, entry) do
    %{state | entries: Map.put(state.entries, pubkey, entry)}
  end

  defp source_pubkey(%Identity{public_key: pubkey})
       when is_binary(pubkey) and byte_size(pubkey) == 32,
       do: {:ok, pubkey}

  defp source_pubkey(pubkey) when is_binary(pubkey) and byte_size(pubkey) == 32, do: {:ok, pubkey}
  defp source_pubkey(_), do: {:error, :invalid_identity}
end
