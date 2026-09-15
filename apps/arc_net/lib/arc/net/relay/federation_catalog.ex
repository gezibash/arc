defmodule Arc.Net.Relay.FederationCatalog do
  @moduledoc false

  # An in-memory, pull-based catalog exchanged only between configured relay
  # peers. Announcements remain the authority: catalog state is an expiring
  # cache of verified signed records, never a new identity namespace.

  alias Arc.Data.RelayAnnouncement

  @max_records 10_000
  @max_page 50
  @max_reply_bytes 220 * 1024
  @max_journal 256
  @snapshot_lease 60
  @peer_lease 30
  @quota_resnapshot_interval 30

  @type catalog :: map()

  def new(<<_::binary-size(32)>> = home, peer_keys, opts \\ []) when is_list(peer_keys) do
    peers = peer_keys |> Enum.filter(&key?/1) |> MapSet.new()

    if MapSet.size(peers) != length(peer_keys) or MapSet.member?(peers, home) do
      raise ArgumentError, "catalog peers must be distinct 32-byte keys other than home"
    end

    %{
      home: home,
      peers: peers,
      epoch: random_epoch(),
      revision: 0,
      generation: 0,
      opts: %{
        max_records: Keyword.get(opts, :max_records, @max_records),
        max_page: Keyword.get(opts, :max_page, @max_page),
        max_reply_bytes: Keyword.get(opts, :max_reply_bytes, @max_reply_bytes),
        snapshot_lease: Keyword.get(opts, :snapshot_lease, @snapshot_lease),
        peer_lease: Keyword.get(opts, :peer_lease, @peer_lease)
      },
      local: %{},
      imports: %{},
      exports: %{},
      export_truncated: %{},
      export_revisions: %{},
      journals: %{},
      snapshots: %{},
      peers_state: %{},
      truncated?: false
    }
  end

  def rebuild(catalog, local_verified_entries, transit, now \\ System.system_time(:second))
      when is_map(catalog) and is_list(local_verified_entries) and is_boolean(transit) and
             is_integer(now) do
    original = catalog
    {imports, peers_state} = expire_peers(catalog.imports, catalog.peers_state, now)
    local = local_entries(local_verified_entries, catalog.home, now)
    imports = prune_imports(imports, peers_state, now)

    catalog = %{
      catalog
      | local: local,
        imports: imports,
        peers_state: peers_state,
        snapshots: prune_snapshots(catalog.snapshots, now)
    }

    catalog = enforce_import_quota(catalog)

    Enum.reduce(catalog.peers, catalog, fn peer, acc ->
      {view, view_truncated?} = export_view(acc, peer, transit)
      previous = Map.get(acc.exports, peer, %{})

      if view == previous and Map.get(acc.export_truncated, peer, false) == view_truncated? do
        acc
      else
        events = diff_events(previous, view, Map.get(acc.export_revisions, peer, 0))

        revision =
          if events == [],
            do: Map.get(acc.export_revisions, peer, 0),
            else: List.last(events).revision

        %{
          acc
          | revision: revision,
            generation: acc.generation + 1,
            exports: Map.put(acc.exports, peer, view),
            export_truncated: Map.put(acc.export_truncated, peer, view_truncated?),
            export_revisions: Map.put(acc.export_revisions, peer, revision),
            journals:
              Map.update(acc.journals, peer, trim_journal(events), &trim_journal(&1 ++ events))
        }
      end
    end)
    |> bump_for_state_change(original)
  end

  def request(catalog, peer, request_map) when is_map(catalog) and is_map(request_map) do
    request_map = normalize_nulls(request_map)
    now = System.system_time(:second)

    with true <- MapSet.member?(catalog.peers, peer),
         {:ok, request} <- valid_request(request_map),
         {:ok, reply, catalog} <- reply_for(catalog, peer, request, now) do
      {reply, catalog}
    else
      _ -> {error_reply(catalog), catalog}
    end
  rescue
    _ -> {error_reply(catalog), catalog}
  end

  def next_request(catalog, peer, now \\ System.system_time(:second)) when is_map(catalog) do
    state = Map.get(catalog.peers_state, peer, %{})

    resnapshot? =
      state[:quota_truncated?] == true and
        now >= Map.get(state, :snapshot_at, 0) + @quota_resnapshot_interval

    if state[:snapshot] do
      snapshot = state.snapshot

      %{
        "type" => "catalog",
        "version" => 1,
        "mode" => "snapshot",
        "epoch" => snapshot.epoch,
        "revision" => snapshot.revision,
        "token" => snapshot.token,
        "after" => snapshot.after
      }
    else
      %{
        "type" => "catalog",
        "version" => 1,
        "mode" => if(state[:synced] == true and not resnapshot?, do: "delta", else: "snapshot"),
        "epoch" => if(resnapshot?, do: :null, else: state[:epoch] || :null),
        "revision" => if(resnapshot?, do: 0, else: state[:revision] || 0),
        "token" => :null,
        "after" => :null
      }
    end
  end

  def apply_reply(catalog, peer, request, reply)
      when is_map(catalog) and is_map(request) and is_map(reply) do
    request = normalize_nulls(request)
    reply = normalize_nulls(reply)
    now = System.system_time(:second)

    with true <- MapSet.member?(catalog.peers, peer),
         {:ok, request} <- valid_request(request),
         {:ok, reply} <- valid_reply(reply, request),
         {:ok, catalog, more?} <- apply_valid_reply(catalog, peer, request, reply, now) do
      {:ok, catalog, more?}
    else
      _ -> {:error, fail_peer(catalog, peer), false}
    end
  rescue
    _ -> {:error, fail_peer(catalog, peer), false}
  end

  def fail_peer(catalog, peer) when is_map(catalog) do
    imports = Map.delete(catalog.imports, peer)
    state = Map.put(catalog.peers_state, peer, %{synced: false, revision: 0, epoch: nil})
    updated = %{catalog | imports: imports, peers_state: state} |> enforce_import_quota()
    bump_for_state_change(updated, catalog)
  end

  def entries(catalog) do
    catalog.imports
    |> Enum.flat_map(fn {peer, sources} ->
      Enum.map(sources, fn {_key, entry} -> {peer, entry} end)
    end)
    |> Enum.reject(fn {_peer, entry} -> Map.has_key?(catalog.local, hex(entry.public_key)) end)
    |> Enum.sort_by(fn {peer, entry} ->
      {length(entry.relay_path), entry.relay_path, peer, entry.public_key}
    end)
  end

  def partial?(catalog) do
    catalog.truncated? or
      Enum.any?(catalog.peers, fn peer ->
        state = Map.get(catalog.peers_state, peer, %{})
        state[:synced] != true or state[:truncated?] == true
      end)
  end

  def synced_peers(catalog) do
    Enum.count(catalog.peers, fn peer ->
      get_in(catalog, [:peers_state, peer, :synced]) == true
    end)
  end

  # -- server replies -------------------------------------------------------

  defp reply_for(catalog, peer, %{"mode" => "snapshot"} = request, now) do
    {snapshot, catalog} = snapshot_for(catalog, peer, request, now)
    page = page_snapshot(snapshot, request["after"], catalog.opts)

    reply =
      %{
        "type" => "catalog_reply",
        "version" => 1,
        "mode" => "snapshot",
        "epoch" => catalog.epoch,
        "revision" => snapshot.revision,
        "token" => snapshot.token,
        "records" => Enum.map(page.entries, & &1.record),
        "routes" => routes(page.entries),
        "next" => page.next || :null,
        "truncated" => snapshot.truncated?
      }

    {:ok, reply, catalog}
  end

  defp reply_for(
         catalog,
         peer,
         %{"mode" => "delta", "epoch" => epoch, "revision" => revision} = request,
         _now
       ) do
    journal = Map.get(catalog.journals, peer, [])

    current_revision = Map.get(catalog.export_revisions, peer, 0)

    if epoch == catalog.epoch and delta_available?(journal, revision, current_revision) do
      events = Enum.filter(journal, &(&1.revision > revision))

      reply = %{
        "type" => "catalog_reply",
        "version" => 1,
        "mode" => "delta",
        "epoch" => catalog.epoch,
        "base_revision" => revision,
        "revision" => current_revision,
        "events" => Enum.map(events, &wire_event/1),
        "truncated" => Map.get(catalog.export_truncated, peer, false)
      }

      if reply_bytes(reply) <= catalog.opts.max_reply_bytes do
        {:ok, reply, catalog}
      else
        reset_snapshot_reply(catalog, peer, request)
      end
    else
      reset_snapshot_reply(catalog, peer, request)
    end
  end

  defp reset_snapshot_reply(catalog, peer, request) do
    {:ok, reply, catalog} =
      reply_for(
        catalog,
        peer,
        Map.put(request_for_snapshot(request), "mode", "snapshot"),
        System.system_time(:second)
      )

    {:ok, Map.put(reply, "reset", true), catalog}
  end

  defp request_for_snapshot(request),
    do: Map.merge(request, %{"token" => :null, "after" => :null})

  defp snapshot_for(catalog, peer, request, now) do
    existing = get_in(catalog, [:snapshots, peer])

    snapshot =
      if valid_snapshot?(existing, request, now) do
        %{existing | expires_at: now + catalog.opts.snapshot_lease}
      else
        entries = catalog.exports |> Map.get(peer, %{}) |> Map.values() |> sort_entries()

        %{
          token: random_epoch(),
          epoch: catalog.epoch,
          revision: Map.get(catalog.export_revisions, peer, 0),
          entries: entries,
          expires_at: now + catalog.opts.snapshot_lease,
          truncated?: Map.get(catalog.export_truncated, peer, false)
        }
      end

    {snapshot, put_in(catalog, [:snapshots, peer], snapshot)}
  end

  defp valid_snapshot?(snapshot, request, now) when is_map(snapshot) do
    request["token"] == snapshot.token and request["epoch"] == snapshot.epoch and
      request["revision"] == snapshot.revision and snapshot.expires_at > now
  end

  defp valid_snapshot?(_, _, _), do: false

  # -- client reply application --------------------------------------------

  defp apply_valid_reply(catalog, peer, %{"mode" => "snapshot"} = request, reply, now) do
    apply_snapshot_reply(catalog, peer, request, reply, now)
  end

  defp apply_valid_reply(
         catalog,
         peer,
         %{"mode" => "delta"},
         %{"mode" => "snapshot", "reset" => true} = reply,
         now
       ) do
    reset_request = %{
      "mode" => "snapshot",
      "token" => :null,
      "epoch" => :null,
      "revision" => 0,
      "after" => :null
    }

    original = catalog

    catalog = %{
      catalog
      | imports: Map.delete(catalog.imports, peer),
        peers_state: Map.delete(catalog.peers_state, peer)
    }

    catalog = bump_for_state_change(catalog, original)

    apply_snapshot_reply(catalog, peer, reset_request, reply, now)
  end

  defp apply_valid_reply(catalog, peer, %{"mode" => "delta"} = request, reply, now) do
    state = Map.get(catalog.peers_state, peer, %{})

    with true <- state[:synced] == true,
         true <- state[:epoch] == request["epoch"] and request["epoch"] == reply["epoch"],
         true <-
           state[:revision] == request["revision"] and
             reply["base_revision"] == request["revision"],
         {:ok, changes} <- verify_events(reply["events"], peer, state.revision, [catalog.home]),
         true <-
           List.last([state.revision | Enum.map(changes, & &1.revision)]) == reply["revision"] do
      original = catalog
      imports = Map.get(catalog.imports, peer, %{}) |> apply_events(changes)

      peer_state =
        state
        |> Map.put(:revision, reply["revision"])
        |> Map.put(:lease_expires, now + catalog.opts.peer_lease)
        |> Map.put(:truncated?, reply["truncated"])
        |> Map.put(:snapshot, nil)

      updated = %{catalog | peers_state: Map.put(catalog.peers_state, peer, peer_state)}
      updated = put_imports(updated, peer, imports)
      {:ok, bump_for_state_change(updated, original), false}
    else
      _ -> :error
    end
  end

  defp apply_snapshot_reply(catalog, peer, request, reply, now) do
    state = Map.get(catalog.peers_state, peer, %{})
    stage = state[:snapshot]

    with true <-
           request["token"] in [nil, :null] or (is_map(stage) and request["token"] == stage.token),
         true <- request["epoch"] in [nil, :null] or request["epoch"] == reply["epoch"],
         true <- request["revision"] in [0, reply["revision"]],
         {:ok, received} <-
           verify_records(reply["records"], reply["routes"], peer, [catalog.home]),
         true <- valid_snapshot_page?(reply, received, stage, request["after"]),
         true <- sorted_after?(received, request["after"]),
         true <- snapshot_token_ok?(reply, stage) do
      staged = if(stage, do: stage.entries, else: %{})

      other_staged_count =
        catalog.peers_state
        |> Enum.reject(fn {source, _state} -> source == peer end)
        |> Enum.reduce(0, fn {_source, state}, count ->
          count + if(is_map(state[:snapshot]), do: map_size(state.snapshot.entries), else: 0)
        end)

      {staged, staged_truncated?} =
        stage_entries(
          staged,
          received,
          max(catalog.opts.max_records - other_staged_count, 0),
          is_map(stage) and stage[:truncated?] == true
        )

      if reply["next"] in [nil, :null] do
        original = catalog

        peer_state = %{
          synced: true,
          epoch: reply["epoch"],
          revision: reply["revision"],
          lease_expires: now + catalog.opts.peer_lease,
          truncated?: reply["truncated"],
          quota_truncated?: staged_truncated?,
          snapshot_at: now
        }

        updated = %{catalog | peers_state: Map.put(catalog.peers_state, peer, peer_state)}
        updated = put_imports(updated, peer, staged)
        {:ok, bump_for_state_change(updated, original), false}
      else
        snapshot = %{
          token: reply["token"],
          epoch: reply["epoch"],
          revision: reply["revision"],
          after: reply["next"],
          entries: staged,
          truncated?: staged_truncated?
        }

        peer_state = Map.put(state, :snapshot, snapshot)
        {:ok, %{catalog | peers_state: Map.put(catalog.peers_state, peer, peer_state)}, true}
      end
    else
      _ -> :error
    end
  end

  # -- validation -----------------------------------------------------------

  defp valid_request(%{"type" => "catalog", "version" => 1, "mode" => mode} = request)
       when mode in ["snapshot", "delta"] do
    allowed = ~w(type version mode epoch revision token after)

    if Map.keys(request) |> Enum.all?(&(&1 in allowed)) and is_integer(request["revision"]) and
         request["revision"] >= 0 and nullable_hex?(request["epoch"], 16) and
         nullable_hex?(request["token"], 16) and nullable_hex?(request["after"], 32) do
      {:ok, request}
    else
      :error
    end
  end

  defp valid_request(_), do: :error

  defp valid_reply(
         %{"type" => "catalog_reply", "version" => 1, "mode" => "snapshot"} = reply,
         request
       ) do
    reset? = request["mode"] == "delta" and reply["reset"] == true

    allowed =
      ~w(type version mode epoch revision token records routes next truncated) ++
        if(reset?, do: ["reset"], else: [])

    if exact_keys?(reply, allowed) and (request["mode"] == "snapshot" or reset?) and
         hex?(reply["epoch"], 16) and
         is_integer(reply["revision"]) and reply["revision"] >= 0 and hex?(reply["token"], 16) and
         is_list(reply["records"]) and length(reply["records"]) <= @max_page and
         is_map(reply["routes"]) and
         nullable_hex?(reply["next"], 32) and is_boolean(reply["truncated"]) do
      {:ok, reply}
    else
      :error
    end
  end

  defp valid_reply(
         %{"type" => "catalog_reply", "version" => 1, "mode" => "delta"} = reply,
         request
       ) do
    allowed = ~w(type version mode epoch base_revision revision events truncated)

    if exact_keys?(reply, allowed) and request["mode"] == "delta" and hex?(reply["epoch"], 16) and
         is_integer(reply["base_revision"]) and is_integer(reply["revision"]) and
         reply["base_revision"] >= 0 and reply["revision"] >= reply["base_revision"] and
         is_list(reply["events"]) and length(reply["events"]) <= @max_journal and
         is_boolean(reply["truncated"]) do
      {:ok, reply}
    else
      :error
    end
  end

  defp valid_reply(_, _), do: :error

  defp verify_records(records, routes, peer, visited) when is_list(records) and is_map(routes) do
    if Map.keys(routes) |> Enum.sort() == Enum.map(records, & &1["public_key"]) |> Enum.sort() do
      Enum.reduce_while(records, {:ok, %{}}, fn record, {:ok, acc} ->
        with {:ok, entry} <- RelayAnnouncement.verify(record),
             {:ok, route} <- decode_route(routes[record["public_key"]]),
             {:ok, path} <- verify_path(route, entry, peer, visited) do
          {:cont, {:ok, Map.put(acc, hex(entry.public_key), Map.put(entry, :relay_path, path))}}
        else
          _ -> {:halt, :error}
        end
      end)
    else
      :error
    end
  end

  defp verify_events(events, peer, previous, visited) do
    Enum.reduce_while(events, {:ok, []}, fn event, {:ok, acc} ->
      with true <-
             is_map(event) and is_integer(event["revision"]) and
               event["revision"] == previous + length(acc) + 1,
           {:ok, change} <- verify_event(event, peer, visited) do
        {:cont, {:ok, acc ++ [Map.put(change, :revision, event["revision"])]}}
      else
        _ -> {:halt, :error}
      end
    end)
  end

  defp verify_event(
         %{"op" => "upsert", "record" => record, "route" => route} = event,
         peer,
         visited
       ) do
    if exact_keys?(event, ~w(revision op record route)) do
      with {:ok, entry} <- RelayAnnouncement.verify(record),
           {:ok, decoded_route} <- decode_route(route),
           {:ok, path} <- verify_path(decoded_route, entry, peer, visited) do
        {:ok,
         %{op: :upsert, key: hex(entry.public_key), entry: Map.put(entry, :relay_path, path)}}
      else
        _ -> :error
      end
    else
      :error
    end
  end

  defp verify_event(%{"op" => "withdraw", "public_key" => public_key} = event, _peer, _visited) do
    if exact_keys?(event, ~w(revision op public_key)) and hex?(public_key, 32),
      do: {:ok, %{op: :withdraw, key: public_key}},
      else: :error
  end

  defp verify_event(_, _, _), do: :error

  defp verify_path(path, entry, peer, visited) when is_list(path) and length(path) in 1..8 do
    with true <- Enum.all?(path, &key?/1),
         true <- Enum.uniq(path) == path,
         true <- hd(path) == peer,
         true <- List.last(path) == entry.relay_public_key,
         true <- length(path) <= 8,
         true <- Enum.all?(path, &(&1 not in visited)),
         true <-
           entry.federation == :network or (entry.federation == :direct and length(path) == 1) do
      {:ok, path}
    else
      _ -> :error
    end
  end

  defp verify_path(_, _, _, _), do: :error

  # -- state helpers --------------------------------------------------------

  defp local_entries(entries, home, now) do
    entries
    |> Enum.reduce(%{}, fn entry, acc ->
      if valid_entry?(entry, now) and RelayAnnouncement.federatable?(entry, home),
        do: Map.put(acc, hex(entry.public_key), Map.put(entry, :relay_path, [home])),
        else: acc
    end)
  end

  defp export_view(catalog, target, transit) do
    imported =
      if transit do
        catalog.imports
        |> Enum.reject(fn {peer, _} -> peer == target end)
        |> Enum.flat_map(fn {_peer, sources} -> Map.values(sources) end)
        |> Enum.filter(fn entry ->
          entry.federation == :network and target not in entry.relay_path
        end)
        |> Enum.map(fn entry -> %{entry | relay_path: [catalog.home | entry.relay_path]} end)
        |> Enum.filter(&(length(&1.relay_path) <= 8))
      else
        []
      end

    candidates =
      (Map.values(catalog.local) ++ imported)
      |> Enum.reject(fn entry -> target in entry.relay_path end)
      |> Enum.reduce(%{}, fn entry, acc -> choose_shorter(acc, entry) end)

    {cap_map(candidates, catalog.opts.max_records),
     map_size(candidates) > catalog.opts.max_records}
  end

  defp choose_shorter(acc, entry) do
    key = hex(entry.public_key)

    case Map.get(acc, key) do
      nil ->
        Map.put(acc, key, entry)

      existing when length(entry.relay_path) < length(existing.relay_path) ->
        Map.put(acc, key, entry)

      existing
      when length(entry.relay_path) == length(existing.relay_path) and
             entry.relay_path < existing.relay_path ->
        Map.put(acc, key, entry)

      _ ->
        acc
    end
  end

  defp diff_events(previous, current, previous_revision) do
    keys = (Map.keys(previous) ++ Map.keys(current)) |> Enum.uniq() |> Enum.sort()

    keys
    |> Enum.flat_map(fn key ->
      case {Map.get(previous, key), Map.get(current, key)} do
        {nil, nil} -> []
        {old, nil} -> [%{op: :withdraw, key: hex(old.public_key)}]
        {nil, entry} -> [%{op: :upsert, key: key, entry: entry}]
        {old, entry} when old.record == entry.record and old.relay_path == entry.relay_path -> []
        {_old, entry} -> [%{op: :upsert, key: key, entry: entry}]
      end
    end)
    |> Enum.with_index(1)
    |> Enum.map(fn {event, offset} -> Map.put(event, :revision, previous_revision + offset) end)
  end

  defp wire_event(%{op: :withdraw, revision: revision, key: key}),
    do: %{"revision" => revision, "op" => "withdraw", "public_key" => key}

  defp wire_event(%{op: :upsert, revision: revision, entry: entry}),
    do: %{
      "revision" => revision,
      "op" => "upsert",
      "record" => entry.record,
      "route" => Enum.map(entry.relay_path, &hex/1)
    }

  defp apply_events(imports, changes) do
    Enum.reduce(changes, imports, fn
      %{op: :upsert, key: key, entry: entry}, acc -> Map.put(acc, key, entry)
      %{op: :withdraw, key: key}, acc -> Map.delete(acc, key)
    end)
  end

  defp put_imports(catalog, peer, imports) do
    %{catalog | imports: Map.put(catalog.imports, peer, imports)} |> enforce_import_quota()
  end

  defp enforce_import_quota(catalog) do
    max = catalog.opts.max_records

    all =
      catalog.imports
      |> Enum.flat_map(fn {peer, entries} ->
        Enum.map(entries, fn {key, entry} -> {peer, key, entry} end)
      end)
      |> Enum.sort_by(fn {peer, key, entry} ->
        {length(entry.relay_path), entry.relay_path, peer, key}
      end)

    kept = Enum.take(all, max)

    imports =
      Enum.reduce(kept, %{}, fn {peer, key, entry}, acc ->
        Map.update(acc, peer, %{key => entry}, &Map.put(&1, key, entry))
      end)

    dropped_peers = all |> Enum.drop(max) |> Enum.map(&elem(&1, 0)) |> Enum.uniq()

    states =
      Enum.reduce(dropped_peers, catalog.peers_state, fn peer, states ->
        Map.update(states, peer, %{quota_truncated?: true}, &Map.put(&1, :quota_truncated?, true))
      end)

    # Keep omissions visible until a fresh source snapshot proves them resolved.
    truncated? = Enum.any?(states, fn {_peer, state} -> state[:quota_truncated?] == true end)
    %{catalog | imports: imports, peers_state: states, truncated?: truncated?}
  end

  defp prune_imports(imports, peers_state, now) do
    imports
    |> Enum.map(fn {peer, entries} ->
      lease = get_in(peers_state, [peer, :lease_expires])
      active? = is_integer(lease) and lease > now

      {peer,
       if(active?,
         do: Enum.filter(entries, fn {_key, entry} -> valid_entry?(entry, now) end) |> Map.new(),
         else: %{}
       )}
    end)
    |> Enum.reject(fn {_peer, entries} -> entries == %{} end)
    |> Map.new()
  end

  defp expire_peers(imports, peers_state, now) do
    Enum.reduce(peers_state, {imports, peers_state}, fn {peer, state}, {imports, states} ->
      if state[:synced] == true and is_integer(state[:lease_expires]) and
           state.lease_expires <= now do
        {Map.delete(imports, peer),
         Map.put(states, peer, %{synced: false, revision: 0, epoch: nil})}
      else
        {imports, states}
      end
    end)
  end

  defp prune_snapshots(snapshots, now),
    do: Enum.filter(snapshots, fn {_peer, snap} -> snap.expires_at > now end) |> Map.new()

  defp trim_journal(events), do: Enum.take(events, -@max_journal)

  defp bump_for_state_change(catalog, original) do
    if catalog.local != original.local or catalog.imports != original.imports or
         catalog.truncated? != original.truncated? or
         peer_health(catalog.peers_state) != peer_health(original.peers_state) do
      %{catalog | generation: catalog.generation + 1}
    else
      catalog
    end
  end

  defp cap_map(map, limit), do: map |> Enum.sort() |> Enum.take(limit) |> Map.new()
  defp sort_entries(entries), do: Enum.sort_by(entries, &hex(&1.public_key))
  defp merge_entries(left, right), do: Map.merge(left, right)

  defp stage_entries(staged, received, limit, prior_truncated?) do
    merged = merge_entries(staged, received)

    kept =
      merged
      |> Map.values()
      |> Enum.sort_by(fn entry ->
        {length(entry.relay_path), entry.relay_path, hex(entry.public_key)}
      end)
      |> Enum.take(limit)
      |> Map.new(fn entry -> {hex(entry.public_key), entry} end)

    {kept, prior_truncated? or map_size(merged) > limit}
  end

  defp routes(entries),
    do:
      Map.new(entries, fn entry -> {hex(entry.public_key), Enum.map(entry.relay_path, &hex/1)} end)

  defp page_snapshot(snapshot, cursor, opts) do
    entries =
      snapshot.entries |> Enum.filter(&(not is_binary(cursor) or hex(&1.public_key) > cursor))

    selected = entries |> Enum.take(opts.max_page) |> fit_page(opts.max_reply_bytes)

    %{
      entries: selected,
      next:
        if(length(entries) > length(selected) and selected != [],
          do: hex(List.last(selected).public_key)
        )
    }
  end

  defp fit_page(entries, max),
    do:
      if(
        :json.encode(%{"records" => Enum.map(entries, & &1.record), "routes" => routes(entries)})
        |> IO.iodata_length() > max,
        do: fit_page(Enum.drop(entries, -1), max),
        else: entries
      )

  defp sorted_after?(entries, cursor),
    do:
      (keys = entries |> Map.keys() |> Enum.sort()) == Map.keys(entries) |> Enum.sort() and
        Enum.all?(keys, &(not is_binary(cursor) or &1 > cursor))

  defp valid_snapshot_page?(reply, received, stage, cursor) do
    raw_keys = Enum.map(reply["records"], &Map.get(&1, "public_key"))
    received_keys = Map.keys(received)
    next = reply["next"]

    Enum.all?(raw_keys, &hex?(&1, 32)) and raw_keys == Enum.sort(raw_keys) and
      length(raw_keys) == length(Enum.uniq(raw_keys)) and
      Enum.sort(raw_keys) == Enum.sort(received_keys) and
      (next in [nil, :null] or (raw_keys != [] and next == List.last(raw_keys))) and
      Enum.all?(raw_keys, &(not is_binary(cursor) or &1 > cursor)) and
      (not is_map(stage) or Enum.all?(raw_keys, &(not Map.has_key?(stage.entries, &1))))
  end

  defp snapshot_token_ok?(_reply, nil), do: true

  defp snapshot_token_ok?(reply, stage),
    do:
      reply["token"] == stage.token and reply["epoch"] == stage.epoch and
        reply["revision"] == stage.revision

  defp delta_available?(_journal, revision, current) when revision == current, do: true
  defp delta_available?([], _revision, _current), do: false

  defp delta_available?(journal, revision, current),
    do: hd(journal).revision <= revision + 1 and List.last(journal).revision == current

  defp valid_entry?(%{expires_at: expires_at}, now),
    do: is_integer(expires_at) and expires_at > now

  defp valid_entry?(_, _), do: false
  defp exact_keys?(map, keys), do: MapSet.new(Map.keys(map)) == MapSet.new(keys)
  defp nullable_hex?(value, bytes), do: value in [nil, :null] or hex?(value, bytes)

  defp hex?(value, bytes) when is_binary(value) and byte_size(value) == bytes * 2,
    do: match?({:ok, _}, Base.decode16(value, case: :lower))

  defp hex?(_, _), do: false
  defp key?(<<_::binary-size(32)>>), do: true
  defp key?(_), do: false

  defp decode_route(route) when is_list(route) and length(route) in 1..8 do
    Enum.reduce_while(route, {:ok, []}, fn value, {:ok, keys} ->
      case Base.decode16(value, case: :lower) do
        {:ok, <<_::binary-size(32)>> = key} -> {:cont, {:ok, keys ++ [key]}}
        _ -> {:halt, :error}
      end
    end)
  end

  defp decode_route(_), do: :error

  defp hex(key), do: Base.encode16(key, case: :lower)
  defp random_epoch, do: :crypto.strong_rand_bytes(16) |> Base.encode16(case: :lower)

  defp normalize_nulls(map) do
    Enum.reduce(~w(epoch token after next), map, fn key, acc ->
      if Map.has_key?(acc, key) and Map.get(acc, key) == nil,
        do: Map.put(acc, key, :null),
        else: acc
    end)
  end

  defp error_reply(catalog),
    do: %{
      "type" => "catalog_reply",
      "version" => 1,
      "error" => "invalid_catalog_request",
      "epoch" => catalog.epoch
    }

  defp reply_bytes(reply), do: reply |> :json.encode() |> IO.iodata_length()

  defp peer_health(states) do
    Map.new(states, fn {peer, state} ->
      {peer, Map.take(state, [:synced, :epoch, :truncated?])}
    end)
  end
end
