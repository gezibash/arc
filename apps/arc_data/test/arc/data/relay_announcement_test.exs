defmodule Arc.Data.RelayAnnouncementTest do
  use ExUnit.Case, async: true

  alias Arc.Data.RelayAnnouncement
  alias Arc.Identity

  @now 1_800_000_000

  test "creates and verifies a deterministic signed capability announcement" do
    identity = identity(1)
    capabilities = [%{"id" => "files", "kind" => "storage", "title" => "Private files"}]

    first = RelayAnnouncement.create(identity, capabilities, now: @now)
    second = RelayAnnouncement.create(identity, capabilities, now: @now)

    assert first == second
    assert {:ok, entry} = RelayAnnouncement.verify(first, now: @now + 1)
    assert entry.public_key == identity.public_key
    assert entry.name == Identity.name(identity)
    assert entry.short_name == Identity.short_name(identity)
    assert entry.capabilities == capabilities
    assert entry.record == first

    assert RelayAnnouncement.provider(entry) == %{
             "public_key" => Identity.encode_public_key(identity),
             "name" => Identity.name(identity),
             "short_name" => Identity.short_name(identity)
           }
  end

  test "accepts the capability list from a manifest summary" do
    identity = identity(2)

    summary = %{
      "manifest_version" => 2,
      "view" => "summary",
      "capabilities" => [
        %{
          "id" => "primary",
          "kind" => "storage",
          "scheme" => "arc",
          "title" => "Private files",
          "summary" => "Encrypted object storage",
          "invocation_mode" => "request_reply",
          "release_version" => "0.1.0",
          "channel" => "stable",
          "detail_path" => "/info/capabilities/primary"
        }
      ]
    }

    assert announcement = RelayAnnouncement.create(identity, summary, now: @now)

    assert {:ok, %{capabilities: [capability]}} =
             RelayAnnouncement.verify(announcement, now: @now)

    assert capability == hd(summary["capabilities"])
  end

  test "uses a distinct, relay-bound signed shape only for direct federation" do
    provider = identity(8)
    home_relay = identity(9).public_key

    local = RelayAnnouncement.create(provider, [capability()], now: @now)
    assert local["version"] == 1
    refute Map.has_key?(local, "federation")
    refute Map.has_key?(local, "relay_public_key")

    direct =
      RelayAnnouncement.create(provider, [capability()],
        now: @now,
        federation: :direct,
        relay_public_key: home_relay
      )

    assert direct["version"] == 2
    assert direct["federation"] == "direct"
    assert direct["relay_public_key"] == Identity.encode_public_key(home_relay)

    assert {:ok, entry} = RelayAnnouncement.verify(direct, now: @now)
    assert entry.federation == :direct
    assert entry.relay_public_key == home_relay
    assert RelayAnnouncement.federatable?(entry, home_relay)
    refute RelayAnnouncement.federatable?(entry, identity(10).public_key)

    assert {:error, :invalid_announcement} =
             RelayAnnouncement.verify(
               Map.put(
                 direct,
                 "relay_public_key",
                 Identity.encode_public_key(identity(10).public_key)
               ),
               now: @now
             )

    assert {:error, :invalid_announcement} =
             RelayAnnouncement.verify(Map.put(direct, "federation", "all"), now: @now)
  end

  test "permits a direct listener without capabilities while keeping it opt-in" do
    home_relay = identity(11).public_key

    direct =
      RelayAnnouncement.create(identity(12), [],
        now: @now,
        federation: :direct,
        relay_public_key: home_relay
      )

    assert {:ok, entry} = RelayAnnouncement.verify(direct, now: @now)
    assert entry.capabilities == []
    assert RelayAnnouncement.federatable?(entry, home_relay)
    refute RelayAnnouncement.search_match?(entry, "")
  end

  test "uses v3 signatures only for onward network federation" do
    provider = identity(13)
    home_relay = identity(14).public_key

    network =
      RelayAnnouncement.create(provider, [capability()],
        now: @now,
        federation: :network,
        relay_public_key: home_relay
      )

    assert network["version"] == 3
    assert network["federation"] == "network"
    assert {:ok, entry} = RelayAnnouncement.verify(network, now: @now)
    assert entry.federation == :network
    assert entry.relay_public_key == home_relay
    assert RelayAnnouncement.federatable?(entry, home_relay)
    refute RelayAnnouncement.federatable?(entry, identity(15).public_key)

    # A valid v3 signature cannot be recast as the older direct-only scope.
    assert {:error, :invalid_announcement} =
             RelayAnnouncement.verify(
               network |> Map.put("version", 2) |> Map.put("federation", "direct"),
               now: @now
             )

    assert {:error, :invalid_announcement} =
             RelayAnnouncement.verify(
               network
               |> Map.put("relay_public_key", Identity.encode_public_key(identity(15).public_key)),
               now: @now
             )
  end

  test "rejects tampering, foreign names, and swapped X25519 keys" do
    first = identity(3)
    second = identity(4)
    announcement = RelayAnnouncement.create(first, [capability()], now: @now)

    assert {:error, :invalid_announcement} =
             RelayAnnouncement.verify(
               put_in(announcement, ["capabilities", Access.at(0), "title"], "Changed"),
               now: @now
             )

    injected = Map.put(announcement, "name", Identity.name(second))
    assert {:error, :invalid_announcement} = RelayAnnouncement.verify(injected, now: @now)

    extra = Map.put(announcement, "x25519_public", String.duplicate("0", 64))
    assert {:error, :invalid_announcement} = RelayAnnouncement.verify(extra, now: @now)
  end

  test "enforces signed times, record bounds, and strict capability fields" do
    identity = identity(5)
    announcement = RelayAnnouncement.create(identity, [capability()], now: @now)

    assert {:ok, _entry} = RelayAnnouncement.verify(announcement, now: @now + 179)

    assert {:error, :invalid_announcement} =
             RelayAnnouncement.verify(announcement, now: @now + 180)

    future = RelayAnnouncement.create(identity, [capability()], now: @now + 31)

    assert {:error, :invalid_announcement} =
             RelayAnnouncement.verify(future, now: @now)

    assert {:error, :invalid_announcement} =
             RelayAnnouncement.verify(Map.put(announcement, "expires_at", @now + 181), now: @now)

    unknown_capability =
      update_in(announcement, ["capabilities", Access.at(0)], &Map.put(&1, "url", "x"))

    assert {:error, :invalid_announcement} =
             RelayAnnouncement.verify(unknown_capability, now: @now)

    oversized = Map.put(announcement, "signature", String.duplicate("a", 16_384))
    assert {:error, :invalid_announcement} = RelayAnnouncement.verify(oversized, now: @now)
  end

  test "matches identities and searches public capability summaries" do
    identity = identity(6)
    announcement = RelayAnnouncement.create(identity, [capability()], now: @now)
    assert {:ok, entry} = RelayAnnouncement.verify(announcement, now: @now)

    assert RelayAnnouncement.matches?(entry, entry.name)
    assert RelayAnnouncement.matches?(entry, entry.short_name)

    assert RelayAnnouncement.matches?(
             entry,
             String.slice(Identity.encode_public_key(identity), 0, 12)
           )

    refute RelayAnnouncement.matches?(entry, "")
    refute RelayAnnouncement.matches?(entry, "not-this-provider")
    assert RelayAnnouncement.search_match?(entry, "encrypted storage")
    assert RelayAnnouncement.search_match?(entry, "storage files")
    assert RelayAnnouncement.search_match?(entry, "")

    empty = RelayAnnouncement.create(identity, [], now: @now)
    assert {:ok, empty_entry} = RelayAnnouncement.verify(empty, now: @now)
    refute RelayAnnouncement.search_match?(empty_entry, "")
  end

  test "rejects invalid input when creating records" do
    assert_raise ArgumentError, fn ->
      RelayAnnouncement.create(identity(7), [%{"id" => "x", "unexpected" => "field"}], now: @now)
    end

    assert_raise ArgumentError, fn ->
      RelayAnnouncement.create(identity(7), [capability()], now: @now, ttl: 181)
    end

    assert_raise ArgumentError, fn ->
      RelayAnnouncement.create(identity(7), List.duplicate(capability(), 9), now: @now)
    end

    assert_raise ArgumentError, fn ->
      RelayAnnouncement.create(identity(7), [capability()], now: @now, federation: :direct)
    end

    assert_raise ArgumentError, fn ->
      RelayAnnouncement.create(identity(7), [capability()], now: @now, federation: :network)
    end

    assert_raise ArgumentError, fn ->
      RelayAnnouncement.create(identity(7), [capability()],
        now: @now,
        relay_public_key: identity(8).public_key
      )
    end

    announcement =
      RelayAnnouncement.create(identity(7), [%{"id" => "files", "summary" => ""}], now: @now)

    assert {:ok, %{capabilities: [%{"id" => "files"}]}} =
             RelayAnnouncement.verify(announcement, now: @now)
  end

  defp capability do
    %{
      "id" => "files",
      "kind" => "storage",
      "scheme" => "arc",
      "title" => "Private files",
      "summary" => "Encrypted object storage",
      "invocation_mode" => "request_reply",
      "release_version" => "0.1.0",
      "channel" => "stable",
      "detail_path" => "/info/capabilities/files"
    }
  end

  defp identity(byte), do: Identity.from_seed(:binary.copy(<<byte>>, 32))
end
