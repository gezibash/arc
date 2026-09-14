defmodule Arc.Control.Local do
  @moduledoc """
  File-backed local control plane provider.

  Persists published identities to ~/.config/arc/control/ so they
  survive across processes. Two separate `arc` invocations can
  discover each other's published identities.
  """

  use GenServer
  @behaviour Arc.Control

  alias Arc.Identity

  @control_dir Path.join(["~", ".config", "arc", "control"])

  def reset do
    GenServer.call(__MODULE__, :reset)
  end

  # --- Client API (behaviour callbacks) ---

  @impl Arc.Control
  def publish(%Identity{} = identity) do
    GenServer.call(__MODULE__, {:publish, identity})
  end

  @impl Arc.Control
  def resolve(query) when is_binary(query) do
    GenServer.call(__MODULE__, {:resolve, query})
  end

  @impl Arc.Control
  def publish_keyex(<<public_key::binary-size(32)>>, x25519_public)
      when is_binary(x25519_public) do
    GenServer.call(__MODULE__, {:publish_keyex, public_key, x25519_public})
  end

  @impl Arc.Control
  def revoke(<<public_key::binary-size(32)>>) do
    GenServer.call(__MODULE__, {:revoke, public_key})
  end

  @impl Arc.Control
  def subscribe(event_type \\ :all) do
    GenServer.call(__MODULE__, {:subscribe, self(), event_type})
  end

  # --- GenServer ---

  def start_link(opts \\ []) do
    name = Keyword.get(opts, :name, __MODULE__)
    GenServer.start_link(__MODULE__, %{}, name: name)
  end

  @impl GenServer
  def init(_) do
    entries = load_entries()
    {:ok, %{entries: entries, subscribers: []}}
  end

  @impl GenServer
  def handle_call({:publish, identity}, _from, state) do
    # Reload from disk so a keyex published by another process survives.
    entries = load_entries()
    existing = Map.get(entries, identity.public_key)

    entry = %{
      public_key: identity.public_key,
      name: Identity.name(identity),
      short_name: Identity.short_name(identity),
      x25519_public: existing && existing.x25519_public,
      published_at: System.monotonic_time(:millisecond),
      status: :active
    }

    state = %{state | entries: Map.put(entries, identity.public_key, entry)}
    persist_entry(entry)
    notify(state.subscribers, {:identity_published, entry})
    {:reply, :ok, state}
  end

  def handle_call({:resolve, query}, _from, state) do
    # Reload from disk to pick up entries from other processes
    entries = load_entries()
    state = %{state | entries: entries}

    results =
      entries
      |> Map.values()
      |> Enum.filter(&(match_entry?(&1, query) and &1.status == :active))

    {:reply, {:ok, results}, state}
  end

  def handle_call({:publish_keyex, public_key, x25519_public}, _from, state) do
    # Reload to get latest
    entries = load_entries()

    case Map.get(entries, public_key) do
      nil ->
        {:reply, {:error, :not_found}, %{state | entries: entries}}

      entry ->
        entry = %{entry | x25519_public: x25519_public}
        entries = Map.put(entries, public_key, entry)
        persist_entry(entry)
        notify(state.subscribers, {:keyex_published, entry})
        {:reply, :ok, %{state | entries: entries}}
    end
  end

  def handle_call({:revoke, public_key}, _from, state) do
    case Map.get(state.entries, public_key) do
      nil ->
        {:reply, {:error, :not_found}, state}

      entry ->
        entry = %{entry | status: :revoked}
        state = put_in(state, [:entries, public_key], entry)
        persist_entry(entry)
        notify(state.subscribers, {:identity_revoked, entry})
        {:reply, :ok, state}
    end
  end

  def handle_call({:subscribe, pid, event_type}, _from, state) do
    ref = Process.monitor(pid)
    sub = %{pid: pid, ref: ref, event_type: event_type}
    {:reply, {:ok, pid}, %{state | subscribers: [sub | state.subscribers]}}
  end

  def handle_call(:reset, _from, _state) do
    clear_entries()
    {:reply, :ok, %{entries: %{}, subscribers: []}}
  end

  @impl GenServer
  def handle_info({:DOWN, ref, :process, _pid, _reason}, state) do
    subs = Enum.reject(state.subscribers, &(&1.ref == ref))
    {:noreply, %{state | subscribers: subs}}
  end

  # --- Persistence ---

  defp control_dir do
    Path.expand(Application.get_env(:arc_control, :control_dir, @control_dir))
  end

  defp persist_entry(entry) do
    dir = control_dir()
    File.mkdir_p!(dir)
    pk_hex = Base.encode16(entry.public_key, case: :lower)
    path = Path.join(dir, "#{pk_hex}.entry")
    File.write!(path, :erlang.term_to_binary(entry))
  end

  defp load_entries do
    dir = control_dir()

    case File.ls(dir) do
      {:ok, files} ->
        files
        |> Enum.filter(&String.ends_with?(&1, ".entry"))
        |> Enum.reduce(%{}, fn file, acc ->
          path = Path.join(dir, file)

          case File.read(path) do
            {:ok, data} ->
              try do
                entry = :erlang.binary_to_term(data, [:safe])
                Map.put(acc, entry.public_key, entry)
              rescue
                _ -> acc
              end

            _ ->
              acc
          end
        end)

      {:error, :enoent} ->
        %{}
    end
  end

  defp clear_entries do
    dir = control_dir()

    case File.ls(dir) do
      {:ok, files} ->
        for file <- files, String.ends_with?(file, ".entry") do
          File.rm(Path.join(dir, file))
        end

      _ ->
        :ok
    end
  end

  # --- Private ---

  defp match_entry?(entry, query) do
    pk_hex = Base.encode16(entry.public_key, case: :lower)

    entry.name == query or
      entry.short_name == query or
      String.starts_with?(pk_hex, String.downcase(query))
  end

  defp notify(subscribers, {event_type, _payload} = message) do
    for %{pid: pid, event_type: sub_type} <- subscribers,
        sub_type == :all or sub_type == event_type do
      send(pid, {:arc_control, message})
    end
  end
end
