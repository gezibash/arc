defmodule Arc.Net.Relay.FederationCatalogTest do
  use ExUnit.Case, async: true

  alias Arc.Data.RelayAnnouncement
  alias Arc.Identity
  alias Arc.Net.Relay.FederationCatalog, as: Catalog

  test "snapshots commit atomically and deltas require a contiguous revision" do
    home = key(1)
    peer = key(2)
    publisher = entry(3, :network, home)
    second_publisher = entry(4, :network, home)
    source = Catalog.new(home, [peer], max_page: 1)
    source = Catalog.rebuild(source, [publisher, second_publisher], false, now())
    target = Catalog.new(peer, [home], max_page: 1)

    request = Catalog.next_request(target, home)
    {reply, source} = Catalog.request(source, peer, request)
    assert {:ok, target, true} = Catalog.apply_reply(target, home, request, reply)
    assert Catalog.entries(target) == []

    request = Catalog.next_request(target, home)
    {reply, _source} = Catalog.request(source, peer, request)
    assert {:ok, target, false} = Catalog.apply_reply(target, home, request, reply)
    assert [{^home, imported}, {^home, _}] = Catalog.entries(target)
    assert imported.public_key in [publisher.public_key, second_publisher.public_key]
    assert Catalog.synced_peers(target) == 1

    request = Catalog.next_request(target, home)

    bad_delta = %{
      "type" => "catalog_reply",
      "version" => 1,
      "mode" => "delta",
      "epoch" => request["epoch"],
      "base_revision" => request["revision"],
      "revision" => request["revision"] + 2,
      "events" => [],
      "truncated" => false
    }

    assert {:error, target, false} = Catalog.apply_reply(target, home, request, bad_delta)
    assert Catalog.entries(target) == []
    assert Catalog.partial?(target)
  end

  test "direct and legacy records are never re-exported beyond their first peer" do
    a = key(10)
    b = key(11)
    c = key(12)
    network = entry(13, :network, a)
    direct = entry(14, :direct, a)
    local = entry(15, :local, a)
    source = Catalog.new(a, [b, c]) |> Catalog.rebuild([network, direct, local], true, now())
    receiver = Catalog.new(b, [a, c])

    {receiver, _} = sync(receiver, source, a, b)
    forwarded = Catalog.rebuild(receiver, [], true, now())
    {reply, _} = Catalog.request(forwarded, c, Catalog.next_request(Catalog.new(c, [b]), b))

    assert reply["records"] == [network.record]

    refute Enum.any?(
             reply["records"],
             &(&1["public_key"] in [direct.record["public_key"], local.record["public_key"]])
           )

    assert Catalog.entries(receiver) != []
  end

  test "rejects forged signature, invalid first hop, and v1 records from a peer" do
    home = key(20)
    peer = key(21)
    target = Catalog.new(home, [peer])
    network = entry(22, :network, peer)
    request = Catalog.next_request(target, peer)

    for {record, route} <- [
          {Map.put(network.record, "signature", String.duplicate("0", 128)), [peer]},
          {network.record, [key(23), peer]},
          {entry(24, :local, peer).record, [peer]}
        ] do
      reply = snapshot_reply(request, record, route)
      assert {:error, failed, false} = Catalog.apply_reply(target, peer, request, reply)
      assert Catalog.entries(failed) == []
    end
  end

  test "keeps shorter alternatives and marks bounded imports partial" do
    home = key(30)
    first = key(31)
    second = key(32)
    provider = entry(33, :network, second)
    target = Catalog.new(home, [first, second], max_records: 1)
    request = Catalog.next_request(target, first)
    reply = snapshot_reply(request, provider.record, [first, second])
    assert {:ok, target, false} = Catalog.apply_reply(target, first, request, reply)

    request = Catalog.next_request(target, second)
    reply = snapshot_reply(request, provider.record, [second])
    assert {:ok, target, false} = Catalog.apply_reply(target, second, request, reply)
    [{peer, entry}] = Catalog.entries(target)
    assert peer == second
    assert entry.relay_path == [second]
    assert Catalog.partial?(target)
  end

  test "expiry removes imports on rebuild" do
    home = key(40)
    peer = key(41)
    expired = entry(42, :network, peer, ttl: 1, now: now() - 2)
    target = Catalog.new(home, [peer])
    request = Catalog.next_request(target, peer)
    reply = snapshot_reply(request, expired.record, [peer])
    assert {:error, target, false} = Catalog.apply_reply(target, peer, request, reply)
    assert Catalog.entries(target) == []
  end

  test "round-tripped delta upserts and withdrawals apply sequentially" do
    home = key(50)
    peer = key(51)
    first = entry(52, :network, home)
    second = entry(53, :network, home)
    source = Catalog.new(home, [peer]) |> Catalog.rebuild([first], false, now())
    target = Catalog.new(peer, [home])
    {target, source} = sync(target, source, home, peer)

    source = Catalog.rebuild(source, [first, second], false, now())
    request = wire(Catalog.next_request(target, home))
    {reply, source} = Catalog.request(source, peer, request)
    assert reply["mode"] == "delta"
    assert {:ok, target, false} = Catalog.apply_reply(target, home, request, wire(reply))

    assert Enum.map(Catalog.entries(target), fn {_peer, entry} -> entry.public_key end)
           |> Enum.sort() ==
             Enum.sort([first.public_key, second.public_key])

    source = Catalog.rebuild(source, [second], false, now())
    request = wire(Catalog.next_request(target, home))
    {reply, _source} = Catalog.request(source, peer, request)
    assert {:ok, target, false} = Catalog.apply_reply(target, home, request, wire(reply))
    assert [{^home, remaining}] = Catalog.entries(target)
    assert remaining.public_key == second.public_key
  end

  defp sync(target, source, source_home, target_home) do
    request = Catalog.next_request(target, source_home) |> wire()
    {reply, source} = Catalog.request(source, target_home, request)
    {:ok, target, more?} = Catalog.apply_reply(target, source_home, request, wire(reply))
    if more?, do: sync(target, source, source_home, target_home), else: {target, source}
  end

  defp snapshot_reply(_request, record, route) do
    %{
      "type" => "catalog_reply",
      "version" => 1,
      "mode" => "snapshot",
      "epoch" => String.duplicate("a", 32),
      "revision" => 1,
      "token" => String.duplicate("b", 32),
      "records" => [record],
      "routes" => %{record["public_key"] => Enum.map(route, &Base.encode16(&1, case: :lower))},
      "next" => :null,
      "truncated" => false
    }
  end

  defp entry(seed, federation, relay, opts \\ []) do
    create_opts =
      if federation == :local,
        do: opts,
        else: [federation: federation, relay_public_key: relay] ++ opts

    record = RelayAnnouncement.create(identity(seed), [%{"id" => "files"}], create_opts)

    {:ok, entry} = RelayAnnouncement.verify(record, now: Keyword.get(opts, :now, now()))
    entry
  end

  defp key(seed), do: identity(seed).public_key
  defp identity(seed), do: Identity.from_seed(:binary.copy(<<seed>>, 32))
  defp now, do: System.system_time(:second)
  defp wire(value), do: value |> :json.encode() |> IO.iodata_to_binary() |> :json.decode()
end
