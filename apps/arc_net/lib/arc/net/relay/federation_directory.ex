defmodule Arc.Net.Relay.FederationDirectory do
  @moduledoc false

  alias Arc.Data.RelayAnnouncement
  alias Arc.Net.Federation

  @max_reply_bytes 220 * 1024
  @max_hops 8
  @query_budget 64

  def valid_request?(%{"type" => type, "query" => query} = request)
      when type in ["resolve", "search"] and is_binary(query) and byte_size(query) <= 256 do
    String.valid?(query) and (type == "search" or query != "") and
      valid_cursor?(request["after"]) and valid_limit?(Map.get(request, "limit", 10)) and
      valid_network?(request["network"])
  end

  def valid_request?(_), do: false

  def originate(request, home) do
    Map.put(request, "network", %{
      "id" => Base.encode16(:crypto.strong_rand_bytes(16), case: :lower),
      "path" => [hex_key(home)],
      "budget" => @query_budget
    })
  end

  def network?(request), do: is_map(request["network"])

  def request_timeout(%{"network" => %{"path" => path}}) when is_list(path),
    do: (9 - min(length(path), @max_hops)) * 1_000 + 250

  def request_timeout(_), do: 1_250

  def valid_ingress?(request, peer, home) do
    valid_request?(request) and
      (not network?(request) or
         (List.last(request["network"]["path"]) == hex_key(peer) and
            hex_key(home) not in request["network"]["path"]))
  end

  def can_continue?(request) do
    network?(request) and length(request["network"]["path"]) < @max_hops and
      request["network"]["budget"] > 1
  end

  def continue(request, home),
    do: update_in(request, ["network", "path"], &(&1 ++ [hex_key(home)]))

  def eligible_peers(peers, request) do
    visited = get_in(request, ["network", "path"]) || []
    peers |> Enum.reject(&(hex_key(&1) in visited)) |> Enum.sort()
  end

  def exportable(entries, home_key, request) do
    direct? = not network?(request) or length(request["network"]["path"]) == 1

    entries
    |> Enum.filter(fn entry ->
      RelayAnnouncement.federatable?(entry, home_key) and
        (direct? or entry.federation == :network)
    end)
    |> Enum.map(&Map.put(&1, :relay_path, [home_key]))
  end

  def export(entries, home_key, request) do
    entries |> exportable(home_key, request) |> matching(request) |> page(request)
  end

  def cached(entries, request) do
    entries |> Enum.uniq_by(& &1.public_key) |> matching(request) |> page(request)
  end

  def query(manager, peer_keys, request) do
    peers = eligible_peers(peer_keys, request)
    budget = if network?(request), do: request["network"]["budget"] - 1, else: length(peers)
    selected = Enum.take(peers, budget)
    timeout = request_timeout(request)

    jobs =
      selected
      |> Enum.with_index()
      |> Enum.map(fn {peer, index} ->
        branch =
          if network?(request) do
            allocation =
              div(budget, length(selected)) +
                if(index < rem(budget, length(selected)), do: 1, else: 0)

            put_in(request, ["network", "budget"], allocation)
          else
            request
          end

        {peer, branch}
      end)

    replies =
      jobs
      |> Task.async_stream(
        fn {peer, branch} -> {peer, Federation.request(manager, peer, branch, timeout)} end,
        max_concurrency: 16,
        timeout: timeout + 250,
        on_timeout: :kill_task,
        ordered: true
      )
      |> Enum.zip(selected)
      |> Enum.map(fn
        {{:ok, {peer, reply}}, _} -> {peer, reply}
        {_, peer} -> {peer, {:error, :federation_timeout}}
      end)

    replies ++ Enum.map(peers -- selected, &{&1, {:error, :federation_budget_exhausted}})
  end

  # Routes are statements by the authenticated partner. The publisher's signed
  # scope and home identity remain authoritative at every hop and at delivery.
  def combine(local, responses, request, opts \\ []) do
    {remote, more?, partial?} =
      Enum.reduce(responses, {[], false, false}, fn {peer, result}, {entries, more, partial} ->
        case validate_reply(peer, result, request) do
          {:ok, verified, next, incomplete} ->
            {Enum.map(verified, &{peer, &1}) ++ entries, more or next != nil,
             partial or incomplete}

          :error ->
            {entries, more, true}
        end
      end)

    exported_remote =
      Enum.map(remote, fn {_, entry} ->
        if home = opts[:home],
          do: Map.update!(entry, :relay_path, &[home | &1]),
          else: entry
      end)

    all =
      (local ++ exported_remote)
      |> Enum.sort_by(fn entry ->
        {length(Map.get(entry, :relay_path, [])), Map.get(entry, :relay_path, [])}
      end)
      |> Enum.uniq_by(& &1.public_key)
      |> matching(request)

    reply = page(all, request)

    next =
      if request["type"] == "search" and more? and reply["entries"] != [],
        do: List.last(reply["entries"])["public_key"],
        else: reply["next"]

    {Map.merge(reply, %{"next" => next, "partial" => partial?}), remote}
  end

  defp validate_reply(peer, {:ok, %{"entries" => records} = reply}, request)
       when is_list(records) do
    limit = if request["type"] == "resolve", do: 2, else: request["limit"] || 10
    next = null_to_nil(reply["next"])
    partial = Map.get(reply, "partial", false)

    with true <- length(records) <= limit,
         true <- valid_cursor?(next) and is_boolean(partial),
         {:ok, entries} <- verify_records(records, reply["routes"], peer, request),
         true <- length(matching(entries, request)) == length(entries),
         keys = Enum.map(entries, &hex/1),
         true <- keys == Enum.sort(Enum.uniq(keys)),
         true <- next == nil or (keys != [] and List.last(keys) == next),
         true <- next == nil or request["type"] == "search" do
      {:ok, entries, next, partial}
    else
      _ -> :error
    end
  end

  defp validate_reply(_, _, _), do: :error

  defp verify_records(records, routes, peer, request) do
    if routes == nil or
         (is_map(routes) and Enum.all?(records, &is_map/1) and
            Enum.sort(Map.keys(routes)) == Enum.sort(Enum.map(records, & &1["public_key"]))) do
      Enum.reduce_while(records, {:ok, []}, fn record, {:ok, entries} ->
        with {:ok, entry} <- RelayAnnouncement.verify(record),
             path <- if(routes, do: routes[record["public_key"]], else: [hex_key(peer)]),
             {:ok, path} <- verify_path(path, entry, peer, request) do
          {:cont, {:ok, entries ++ [Map.put(entry, :relay_path, path)]}}
        else
          _ -> {:halt, :error}
        end
      end)
    else
      :error
    end
  end

  defp verify_path(path, entry, peer, request) when is_list(path) and length(path) in 1..8 do
    visited = get_in(request, ["network", "path"]) || []

    with true <- Enum.all?(path, &valid_key?/1),
         true <- Enum.uniq(path) == path,
         true <- hd(path) == hex_key(peer),
         true <- List.last(path) == hex_key(entry.relay_public_key),
         true <- length(path) + length(visited) <= 9,
         true <- Enum.all?(path, &(&1 not in visited)),
         true <-
           entry.federation == :network or
             (entry.federation == :direct and length(path) == 1 and length(visited) <= 1),
         true <- network?(request) or length(path) == 1 do
      {:ok, Enum.map(path, &Base.decode16!(&1, case: :lower))}
    else
      _ -> :error
    end
  end

  defp verify_path(_, _, _, _), do: :error

  defp matching(entries, request) do
    entries
    |> Enum.filter(fn entry ->
      matches? =
        if request["type"] == "resolve",
          do: RelayAnnouncement.matches?(entry, request["query"]),
          else: RelayAnnouncement.search_match?(entry, request["query"])

      matches? and after_cursor?(entry, request["after"])
    end)
    |> Enum.sort_by(&hex/1)
  end

  defp page(entries, request) do
    limit = if request["type"] == "resolve", do: 2, else: request["limit"] || 10
    selected = entries |> Enum.take(limit) |> fit_page()

    next =
      if request["type"] == "search" and length(entries) > length(selected) and selected != [],
        do: hex(List.last(selected))

    total = if request["type"] == "resolve", do: min(length(entries), 2), else: length(entries)
    Map.merge(page_records(selected), %{"next" => next || :null, "total" => total})
  end

  defp page_records(entries) do
    routes =
      for %{relay_path: path} = entry <- entries,
          into: %{},
          do: {hex(entry), Enum.map(path, &hex_key/1)}

    %{"entries" => Enum.map(entries, & &1.record), "routes" => routes}
  end

  defp fit_page([]), do: []

  defp fit_page(entries) do
    bytes = entries |> page_records() |> :json.encode() |> IO.iodata_length()
    if bytes > @max_reply_bytes, do: fit_page(Enum.drop(entries, -1)), else: entries
  end

  defp valid_network?(nil), do: true

  defp valid_network?(%{"id" => id, "path" => path, "budget" => budget} = network)
       when is_binary(id) and is_list(path) and length(path) in 1..8 and
              is_integer(budget) and budget in 1..64 do
    map_size(network) == 3 and Regex.match?(~r/\A[0-9a-f]{32}\z/, id) and
      Enum.all?(path, &valid_key?/1) and Enum.uniq(path) == path
  end

  defp valid_network?(_), do: false
  defp valid_key?(key) when is_binary(key), do: Regex.match?(~r/\A[0-9a-f]{64}\z/, key)
  defp valid_key?(_), do: false
  defp after_cursor?(entry, cursor) when is_binary(cursor), do: hex(entry) > cursor
  defp after_cursor?(_, _), do: true
  defp valid_limit?(limit), do: is_integer(limit) and limit > 0 and limit <= 50
  defp valid_cursor?(cursor) when cursor in [nil, :null], do: true
  defp valid_cursor?(cursor), do: valid_key?(cursor)
  defp hex(entry), do: hex_key(entry.public_key)
  defp hex_key(key) when is_binary(key), do: Base.encode16(key, case: :lower)
  defp hex_key(_), do: nil
  defp null_to_nil(:null), do: nil
  defp null_to_nil(value), do: value
end
