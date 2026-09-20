defmodule Arc.Net.FederationDirectoryTest do
  use ExUnit.Case, async: true

  alias Arc.Data.RelayAnnouncement
  alias Arc.Identity
  alias Arc.Net.Relay.FederationDirectory, as: Directory

  defp entry(identity, relay \\ nil) do
    opts = if relay, do: [federation: :direct, relay_public_key: relay], else: []
    record = RelayAnnouncement.create(identity, [%{"id" => "files"}], opts)
    {:ok, entry} = RelayAnnouncement.verify(record)
    entry
  end

  test "export requires the publisher's explicit permission for this origin relay" do
    home = Identity.generate().public_key
    other = Identity.generate().public_key
    local = entry(Identity.generate())
    shared = entry(Identity.generate(), home)
    imported = entry(Identity.generate(), other)
    request = %{"type" => "search", "query" => "files", "limit" => 10}

    assert %{"entries" => [record]} = Directory.export([local, shared, imported], home, request)
    assert record == shared.record
    assert %{"entries" => []} = Directory.export([shared], other, request)
  end

  test "merge paginates by identity across local and directly shared providers" do
    home = Identity.generate().public_key
    peer = Identity.generate().public_key
    identities = Enum.sort_by(for(_ <- 1..4, do: Identity.generate()), & &1.public_key)
    [a, b, c, d] = Enum.map(identities, &entry(&1, peer))
    request = %{"type" => "search", "query" => "files", "limit" => 2}
    remote = Directory.export([a, c, d], peer, request)
    {page, _} = Directory.combine([b], [{peer, {:ok, remote}}], request)

    assert page["entries"] == [a.record, b.record]
    assert page["next"] == b.record["public_key"]
    refute page["partial"]

    following = Map.put(request, "after", page["next"])
    remote = Directory.export([a, c, d], peer, following)
    {page, _} = Directory.combine([b], [{peer, {:ok, remote}}], following)
    assert page["entries"] == [c.record, d.record]
    assert page["next"] == :null
    assert Directory.export([b], home, request)["entries"] == []
  end

  test "unavailable or untrustworthy peers make search explicitly partial" do
    peer = Identity.generate().public_key
    other = Identity.generate().public_key
    local = entry(Identity.generate())
    unshared = entry(Identity.generate())
    foreign = entry(Identity.generate(), other)
    request = %{"type" => "search", "query" => "files", "limit" => 10}

    for reply <- [
          {:error, :federation_timeout},
          {:ok, %{"entries" => [unshared.record]}},
          {:ok, %{"entries" => [foreign.record]}},
          {:ok, %{"entries" => %{}}}
        ] do
      {page, imported} = Directory.combine([local], [{peer, reply}], request)
      assert page["partial"]
      assert page["entries"] == [local.record]
      assert imported == []
    end
  end

  test "an unexpired signed result behind the requested cursor is rejected" do
    peer = Identity.generate().public_key
    shared = entry(Identity.generate(), peer)

    request = %{
      "type" => "search",
      "query" => "files",
      "limit" => 10,
      "after" => shared.record["public_key"]
    }

    {page, imported} =
      Directory.combine([], [{peer, {:ok, %{"entries" => [shared.record]}}}], request)

    assert page["partial"]
    assert imported == []
  end
end
