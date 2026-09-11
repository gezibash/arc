defmodule Arc.Host.AgentPool do
  @moduledoc """
  Shared ARC agent pool for the local host service.

  Host clients bind to local identities by name or public-key prefix. The pool
  reuses one `Arc.Data.Agent` per identity inside the host runtime so multiple
  client sessions do not fight over `AgentRegistry` registration.
  """

  use GenServer

  alias Arc.Data.Agent
  alias Arc.Identity
  alias Arc.Identity.KeyStore

  @type relay_opts :: %{host: charlist(), port: pos_integer(), pubkey: binary() | nil} | nil

  def start_link(opts \\ []) do
    GenServer.start_link(__MODULE__, opts)
  end

  def acquire(pool, identity_query \\ nil) do
    GenServer.call(pool, {:acquire, identity_query}, 15_000)
  end

  def resolve_identity(identity_query \\ nil) do
    do_resolve_identity(identity_query)
  end

  def status(pool) do
    GenServer.call(pool, :status)
  end

  @impl GenServer
  def init(opts) do
    relay =
      case Keyword.get(opts, :relay) do
        {host, port} -> %{host: host, port: port, pubkey: Keyword.get(opts, :relay_pubkey)}
        _ -> nil
      end

    {:ok, %{entries: %{}, relay: relay}}
  end

  @impl GenServer
  def handle_call({:acquire, identity_query}, _from, state) do
    with {:ok, identity} <- do_resolve_identity(identity_query),
         {:ok, entry, state} <- ensure_agent(identity, state) do
      {:reply, {:ok, %{identity: identity, agent: entry.agent}}, state}
    else
      {:error, reason} ->
        {:reply, {:error, reason}, state}
    end
  end

  def handle_call(:status, _from, state) do
    identities =
      state.entries
      |> Map.values()
      |> Enum.filter(&(is_pid(&1.agent) and Process.alive?(&1.agent)))
      |> Enum.map(&Identity.name(&1.identity))
      |> Enum.sort()

    {:reply, %{identity_count: length(identities), identities: identities, relay: state.relay},
     state}
  end

  defp ensure_agent(identity, state) do
    pubkey = identity.public_key

    case Map.get(state.entries, pubkey) do
      %{agent: agent} = entry when is_pid(agent) ->
        if Process.alive?(agent) do
          {:ok, entry, state}
        else
          start_agent(identity, state)
        end

      _ ->
        start_agent(identity, state)
    end
  end

  defp start_agent(identity, state) do
    with {:ok, agent} <- Agent.start_link(identity),
         :ok <- Agent.publish(agent),
         :ok <- maybe_connect_relay(identity, state.relay) do
      entry = %{identity: identity, agent: agent}
      {:ok, entry, put_in(state.entries[identity.public_key], entry)}
    else
      {:error, reason} ->
        {:error, reason}
    end
  end

  defp maybe_connect_relay(_identity, nil), do: :ok

  defp maybe_connect_relay(identity, %{host: host, port: port, pubkey: pubkey}) do
    if Code.ensure_loaded?(Arc.Net) and function_exported?(Arc.Net, :connect_relay, 4) do
      # Arc.Net is an optional runtime peer, not a compile-time dep.
      # credo:disable-for-next-line Credo.Check.Refactor.Apply
      apply(Arc.Net, :connect_relay, [host, port, identity, pubkey])
    else
      {:error, :relay_runtime_unavailable}
    end
  end

  defp do_resolve_identity(nil), do: KeyStore.resolve_active()
  defp do_resolve_identity(""), do: KeyStore.resolve_active()

  defp do_resolve_identity(query) when is_binary(query) do
    query = String.trim(query)

    if query == "" do
      KeyStore.resolve_active()
    else
      case KeyStore.get(query) do
        {:ok, identity} ->
          {:ok, identity}

        {:error, :not_found} ->
          resolve_by_public_key_prefix(query)

        {:error, reason} ->
          {:error, reason}
      end
    end
  end

  defp do_resolve_identity(%Identity{} = identity), do: {:ok, identity}

  defp resolve_by_public_key_prefix(query) do
    normalized =
      query
      |> String.trim()
      |> String.trim_leading("0x")
      |> String.downcase()

    matches =
      KeyStore.list()
      |> Enum.filter(fn {_name, identity} ->
        Base.encode16(identity.public_key, case: :lower)
        |> String.starts_with?(normalized)
      end)

    case matches do
      [{_, identity}] -> {:ok, identity}
      [] -> {:error, :not_found}
      _ -> {:error, :ambiguous}
    end
  end
end
