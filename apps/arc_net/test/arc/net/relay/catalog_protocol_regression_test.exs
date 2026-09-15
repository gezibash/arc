defmodule Arc.Net.Relay.CatalogProtocolRegressionTest do
  use ExUnit.Case, async: true

  alias Arc.Data.RelayAnnouncement
  alias Arc.Identity
  alias Arc.Net.Relay.FederationCatalog, as: Catalog

  test "initial snapshot survives a JSON wire round trip with default null fields" do
    home = key(1)
    peer = key(2)
    provider = entry(3, peer)
    source = Catalog.new(peer, [home]) |> Catalog.rebuild([provider], false, now())
    target = Catalog.new(home, [peer])

    request = Catalog.next_request(target, peer)
    assert request["mode"] == "snapshot"
    assert request["epoch"] == :null
    assert request["token"] == :null
    assert request["after"] == :null

    wire_request = wire(request)
    {reply, _source} = Catalog.request(source, home, wire_request)

    assert {:ok, synced, false} = Catalog.apply_reply(target, peer, wire_request, wire(reply))
    assert [{^peer, imported}] = Catalog.entries(synced)
    assert imported.public_key == provider.public_key
  end

  test "a delta applies several revisions including a withdrawal" do
    home = key(10)
    peer = key(11)
    first = entry(12, peer)
    second = entry(13, peer)
    third = entry(14, peer)
    source = Catalog.new(peer, [home]) |> Catalog.rebuild([first], false, now())
    target = Catalog.new(home, [peer])
    {target, source} = sync(target, source, peer, home)

    # The receiver misses all three updates, so one delta must carry them in
    # revision order. This exercises a real withdrawal between upserts.
    source = Catalog.rebuild(source, [first, second], false, now())
    source = Catalog.rebuild(source, [second], false, now())
    source = Catalog.rebuild(source, [second, third], false, now())

    request = Catalog.next_request(target, peer) |> wire()
    {reply, _source} = Catalog.request(source, home, request)
    assert reply["mode"] == "delta"

    assert {:ok, updated, false} = Catalog.apply_reply(target, peer, request, wire(reply))

    assert updated
           |> Catalog.entries()
           |> Enum.map(fn {_source, entry} -> entry.public_key end)
           |> Enum.sort() == Enum.sort([second.public_key, third.public_key])
  end

  test "a changed source epoch falls back to and applies a snapshot" do
    home = key(20)
    peer = key(21)
    original = entry(22, peer)
    replacement = entry(23, peer)
    source = Catalog.new(peer, [home]) |> Catalog.rebuild([original], false, now())
    target = Catalog.new(home, [peer])
    {target, _source} = sync(target, source, peer, home)

    restarted_source = Catalog.new(peer, [home]) |> Catalog.rebuild([replacement], false, now())
    request = Catalog.next_request(target, peer) |> wire()
    {reply, _source} = Catalog.request(restarted_source, home, request)

    assert reply["mode"] == "snapshot"
    assert {:ok, recovered, false} = Catalog.apply_reply(target, peer, request, wire(reply))
    assert [{^peer, imported}] = Catalog.entries(recovered)
    assert imported.public_key == replacement.public_key
  end

  test "accepts a valid eight-hop route and rejects one containing this relay" do
    home = key(30)
    peer = key(31)
    hops = Enum.map(32..37, &key/1)
    destination = key(38)
    provider = entry(39, destination)
    valid_route = [peer | hops] ++ [destination]
    target = Catalog.new(home, [peer])
    request = Catalog.next_request(target, peer)

    assert length(valid_route) == 8

    assert {:ok, accepted, false} =
             Catalog.apply_reply(
               target,
               peer,
               request,
               snapshot_reply(provider.record, valid_route)
             )

    assert [{^peer, imported}] = Catalog.entries(accepted)
    assert imported.relay_path == valid_route

    self_containing_route = [peer, key(32), key(33), home, key(35), key(36), key(37), destination]

    assert {:error, _failed, false} =
             Catalog.apply_reply(
               Catalog.new(home, [peer]),
               peer,
               request,
               snapshot_reply(provider.record, self_containing_route)
             )
  end

  test "an empty delta preserves catalog generation" do
    home = key(50)
    peer = key(51)
    provider = entry(52, peer)
    source = Catalog.new(peer, [home]) |> Catalog.rebuild([provider], false, now())
    target = Catalog.new(home, [peer])
    {target, source} = sync(target, source, peer, home)
    generation = target.generation

    request = Catalog.next_request(target, peer) |> wire()
    {reply, _source} = Catalog.request(source, home, request)
    assert reply["mode"] == "delta"
    assert reply["events"] == []

    assert {:ok, unchanged, false} = Catalog.apply_reply(target, peer, request, wire(reply))
    assert unchanged.generation == generation
  end

  defp sync(target, source, source_home, target_home) do
    request = Catalog.next_request(target, source_home) |> wire()
    {reply, source} = Catalog.request(source, target_home, request)
    {:ok, target, more?} = Catalog.apply_reply(target, source_home, request, wire(reply))
    if more?, do: sync(target, source, source_home, target_home), else: {target, source}
  end

  defp snapshot_reply(record, route) do
    %{
      "type" => "catalog_reply",
      "version" => 1,
      "mode" => "snapshot",
      "epoch" => String.duplicate("a", 32),
      "revision" => 1,
      "token" => String.duplicate("b", 32),
      "records" => [record],
      "routes" => %{record["public_key"] => Enum.map(route, &hex/1)},
      "next" => :null,
      "truncated" => false
    }
  end

  defp entry(seed, relay) do
    record =
      RelayAnnouncement.create(identity(seed), [%{"id" => "files"}],
        federation: :network,
        relay_public_key: relay
      )

    {:ok, entry} = RelayAnnouncement.verify(record, now: now())
    entry
  end

  defp key(seed), do: identity(seed).public_key
  defp identity(seed), do: Identity.from_seed(:binary.copy(<<seed>>, 32))
  defp hex(key), do: Base.encode16(key, case: :lower)
  defp now, do: System.system_time(:second)
  defp wire(value), do: value |> :json.encode() |> IO.iodata_to_binary() |> :json.decode()
end
