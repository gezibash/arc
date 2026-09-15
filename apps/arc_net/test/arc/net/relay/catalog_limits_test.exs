defmodule Arc.Net.Relay.CatalogLimitsTest do
  use ExUnit.Case, async: true
  alias Arc.Data.RelayAnnouncement
  alias Arc.Identity
  alias Arc.Net.Relay.FederationCatalog, as: Catalog

  test "quota omissions stay partial and a later snapshot refills newly free space" do
    home = Identity.generate().public_key
    peer = Identity.generate().public_key
    records = for _ <- 1..2, do: entry(peer)
    source = Catalog.new(peer, [home]) |> Catalog.rebuild(records, false)
    target = Catalog.new(home, [peer], max_records: 1)
    {target, source} = sync(target, source, peer, home)
    [{^peer, retained}] = Catalog.entries(target)
    target = Catalog.rebuild(target, [], false)
    assert Catalog.partial?(target)
    surviving = Enum.reject(records, &(&1.public_key == retained.public_key))
    source = Catalog.rebuild(source, surviving, false)
    {target, source} = sync(target, source, peer, home)
    assert Catalog.entries(target) == []
    assert Catalog.partial?(Catalog.rebuild(target, [], false))

    req = Catalog.next_request(target, peer, System.system_time(:second) + 31)
    assert req["mode"] == "snapshot"
    {reply, _} = Catalog.request(source, home, wire(req))
    assert {:ok, refilled, false} = Catalog.apply_reply(target, peer, req, wire(reply))
    assert [{^peer, restored}] = Catalog.entries(refilled)
    assert restored.public_key == hd(surviving).public_key
    refute Catalog.partial?(Catalog.rebuild(refilled, [], false))
  end

  test "unfinished snapshots share a bounded staging pool" do
    home = Identity.generate().public_key
    peers = for _ <- 1..3, do: Identity.generate().public_key
    target = Catalog.new(home, peers, max_records: 2)

    sources =
      Map.new(peers, fn peer ->
        entries = for _ <- 1..3, do: entry(peer)
        {peer, Catalog.new(peer, [home], max_page: 1) |> Catalog.rebuild(entries, false)}
      end)

    {target, sources} =
      Enum.reduce(peers, {target, sources}, fn peer, {target, sources} ->
        request = Catalog.next_request(target, peer)
        {reply, source} = Catalog.request(sources[peer], home, wire(request))
        assert {:ok, updated, true} = Catalog.apply_reply(target, peer, request, wire(reply))
        assert bounded?(updated, 2)
        {updated, Map.put(sources, peer, source)}
      end)

    assert Catalog.entries(target) == []

    Enum.reduce(peers, target, fn peer, target ->
      {updated, _} = sync(target, sources[peer], peer, home)
      assert bounded?(updated, 2)
      updated
    end)
  end

  test "a delta larger than the control budget resets to bounded snapshot pages" do
    home = Identity.generate().public_key
    peer = Identity.generate().public_key
    source = Catalog.new(peer, [home]) |> Catalog.rebuild([], false)
    {target, source} = sync(Catalog.new(home, [peer]), source, peer, home)
    entries = for _ <- 1..180, do: entry(peer, String.duplicate("x", 1_024))
    source = Catalog.rebuild(source, entries, false)
    request = Catalog.next_request(target, peer)
    {reply, source} = Catalog.request(source, home, wire(request))
    assert %{"mode" => "snapshot", "reset" => true} = reply
    assert reply |> :json.encode() |> IO.iodata_length() < 256 * 1_024
    assert {:ok, target, true} = Catalog.apply_reply(target, peer, request, wire(reply))
    {target, _} = sync(target, source, peer, home)
    assert length(Catalog.entries(target)) == length(entries)
  end

  test "compacted delta history cannot silently omit early records" do
    home = Identity.generate().public_key
    peer = Identity.generate().public_key
    source = Catalog.new(peer, [home]) |> Catalog.rebuild([], false)
    {target, source} = sync(Catalog.new(home, [peer]), source, peer, home)
    entries = for _ <- 1..270, do: entry(peer)
    source = Catalog.rebuild(source, entries, false)
    request = Catalog.next_request(target, peer)
    {reply, source} = Catalog.request(source, home, wire(request))
    assert %{"mode" => "snapshot", "reset" => true} = reply
    assert {:ok, target, true} = Catalog.apply_reply(target, peer, request, wire(reply))
    {target, _} = sync(target, source, peer, home)
    assert length(Catalog.entries(target)) == length(entries)
  end

  test "expired partner leases drop still-valid publications and require resynchronization" do
    home = Identity.generate().public_key
    peer = Identity.generate().public_key
    source = Catalog.new(peer, [home]) |> Catalog.rebuild([entry(peer)], false)
    {target, _} = sync(Catalog.new(home, [peer]), source, peer, home)
    generation = target.generation
    target = Catalog.rebuild(target, [], false, System.system_time(:second) + 31)
    assert Catalog.entries(target) == []
    assert Catalog.synced_peers(target) == 0
    assert Catalog.partial?(target)
    assert Catalog.next_request(target, peer)["mode"] == "snapshot"
    assert target.generation > generation
  end

  test "repeated failure of an already unavailable peer does not invalidate unrelated routes" do
    home = Identity.generate().public_key
    peer = Identity.generate().public_key
    other = Identity.generate().public_key
    source = Catalog.new(peer, [home]) |> Catalog.rebuild([entry(peer)], false)
    {target, _} = sync(Catalog.new(home, [peer, other]), source, peer, home)
    failed = Catalog.fail_peer(target, other)
    retried = Catalog.fail_peer(failed, other)
    assert retried.generation == failed.generation
    assert Catalog.entries(retried) == Catalog.entries(target)
    removed = Catalog.fail_peer(retried, peer)
    assert removed.generation > retried.generation
    assert Catalog.entries(removed) == []
  end

  defp sync(target, source, peer, home) do
    request = Catalog.next_request(target, peer)
    {reply, source} = Catalog.request(source, home, wire(request))
    assert {:ok, target, more?} = Catalog.apply_reply(target, peer, request, wire(reply))
    assert bounded?(target, target.opts.max_records)
    if more?, do: sync(target, source, peer, home), else: {target, source}
  end

  defp bounded?(catalog, limit) do
    staged =
      Enum.reduce(catalog.peers_state, 0, fn {_peer, state}, count ->
        count + if(is_map(state[:snapshot]), do: map_size(state.snapshot.entries), else: 0)
      end)

    length(Catalog.entries(catalog)) <= limit and staged <= limit
  end

  defp entry(peer, summary \\ "") do
    record =
      RelayAnnouncement.create(Identity.generate(), [%{"id" => "files", "summary" => summary}],
        federation: :network,
        relay_public_key: peer
      )

    {:ok, entry} = RelayAnnouncement.verify(record)
    entry
  end

  defp wire(value), do: value |> :json.encode() |> IO.iodata_to_binary() |> :json.decode()
end
