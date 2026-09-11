defmodule Arc.Data.CapabilityDiscovery do
  @moduledoc """
  Query and expansion helpers for ARC capability discovery.

  Discovery stays bounded by fetching only per-peer summary documents during
  search and deferring detail expansion until a capability is explicitly
  selected.

  This module is designed for a dedicated discovery client agent that performs
  serial requests. It consumes the agent inbox while awaiting replies.
  """

  alias Arc.Control
  alias Arc.Data.Agent
  alias Arc.Data.CapabilityManifest
  alias Arc.Data.Frame

  @default_limit 10
  @default_timeout_ms 10_000

  @type match :: %{
          provider: map(),
          capability: map()
        }

  @type result :: %{
          query: String.t(),
          total: non_neg_integer(),
          truncated?: boolean(),
          matches: [match()]
        }

  @spec discover(pid(), String.t(), keyword()) :: {:ok, result()} | {:error, term()}
  def discover(agent, query \\ "", opts \\ []) when is_binary(query) and is_list(opts) do
    limit = Keyword.get(opts, :limit, @default_limit)

    with {:ok, entries} <- discoverable_entries(agent) do
      matches =
        entries
        |> Enum.reduce([], fn entry, acc ->
          case fetch_summary(agent, entry.name, opts) do
            {:ok, %{"provider" => provider, "capabilities" => capabilities}} ->
              acc ++ match_capabilities(provider, capabilities, query)

            _ ->
              acc
          end
        end)
        |> Enum.sort_by(fn %{provider: provider, capability: capability} ->
          {provider["name"] || "", capability["id"] || ""}
        end)

      {:ok,
       %{
         query: query,
         total: length(matches),
         truncated?: length(matches) > limit,
         matches: Enum.take(matches, limit)
       }}
    end
  end

  @spec fetch_summary(pid(), String.t(), keyword()) :: {:ok, map()} | {:error, term()}
  def fetch_summary(agent, peer_query, opts \\ []) when is_binary(peer_query) do
    request_document(agent, peer_query, CapabilityManifest.summary_path(), opts)
  end

  @spec fetch_detail(pid(), String.t(), String.t(), keyword()) :: {:ok, map()} | {:error, term()}
  def fetch_detail(agent, peer_query, capability_id, opts \\ [])
      when is_binary(peer_query) and is_binary(capability_id) do
    request_document(agent, peer_query, CapabilityManifest.detail_path(capability_id), opts)
  end

  defp request_document(agent, peer_query, path, opts) do
    timeout_ms = Keyword.get(opts, :timeout_ms, @default_timeout_ms)

    with {:ok, _entry} <- Agent.connect(agent, peer_query),
         request_id <- Frame.new_request_id(),
         :ok <-
           Agent.send_message(agent, peer_query, "",
             request_id: request_id,
             meta: %{"method" => "GET", "path" => path}
           ),
         {:ok, reply} <- wait_for_reply(agent, request_id, timeout_ms) do
      decode_document(reply)
    end
  end

  defp discoverable_entries(agent) do
    # The current 5-op control plane has no explicit list call. With the local
    # provider, resolve("") enumerates active entries via prefix matching.
    with {:ok, entries} <- Control.resolve(""),
         local_pk <- Agent.info(agent).public_key do
      {:ok, Enum.reject(entries, &(&1.public_key == local_pk))}
    end
  end

  defp match_capabilities(provider, capabilities, query) when is_list(capabilities) do
    tokens = tokenize(query)

    capabilities
    |> Enum.filter(&match_query?(provider, &1, tokens))
    |> Enum.map(fn capability ->
      %{provider: provider, capability: capability}
    end)
  end

  defp match_capabilities(_provider, _capabilities, _query), do: []

  defp match_query?(_provider, _capability, []), do: true

  defp match_query?(provider, capability, tokens) do
    haystack =
      [
        provider["name"],
        provider["short_name"],
        capability["id"],
        capability["kind"],
        capability["scheme"],
        capability["title"],
        capability["summary"]
      ]
      |> Enum.map_join(" ", &normalize_fragment/1)

    Enum.all?(tokens, &String.contains?(haystack, &1))
  end

  defp tokenize(query) do
    query
    |> String.downcase()
    |> String.split(~r/\s+/, trim: true)
  end

  defp normalize_fragment(value) when is_binary(value), do: String.downcase(value)
  defp normalize_fragment(_value), do: ""

  defp wait_for_reply(agent, request_id, timeout_ms) do
    started_at = System.monotonic_time(:millisecond)
    wait_for_reply(agent, request_id, timeout_ms, started_at)
  end

  defp wait_for_reply(agent, request_id, timeout_ms, started_at) do
    Process.sleep(100)
    Agent.poll_mailbox(agent)
    Process.sleep(10)

    matcher = fn message ->
      message[:request_id] == request_id and message[:kind] in [:response, :error]
    end

    case Agent.take_inbox(agent, matcher) do
      [] ->
        if elapsed_ms(started_at) > timeout_ms do
          {:error, :timeout}
        else
          wait_for_reply(agent, request_id, timeout_ms, started_at)
        end

      [match | _] ->
        {:ok, match}
    end
  end

  defp elapsed_ms(started_at) do
    System.monotonic_time(:millisecond) - started_at
  end

  defp decode_document(%{kind: :error, error_code: code, error_message: message}) do
    {:error, {:remote, code || "error", message || "unknown error"}}
  end

  defp decode_document(%{kind: :response, text: text}) do
    {:ok, :json.decode(text)}
  rescue
    _ -> {:error, :invalid_json}
  end

  defp decode_document(_reply), do: {:error, :unexpected_reply}
end
