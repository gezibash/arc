defmodule Arc.Net.NetworkFederationDirectoryTest do
  use ExUnit.Case, async: true

  alias Arc.Data.RelayAnnouncement
  alias Arc.Identity
  alias Arc.Net.Relay.FederationDirectory, as: Directory

  @base_request %{"type" => "search", "query" => "files", "limit" => 10}

  test "only network announcements continue beyond the first federation edge" do
    home = key(1)
    first_peer = key(2)
    local = announcement(3, :local, home)
    direct = announcement(4, :direct, home)
    network = announcement(5, :network, home)

    direct_reply = Directory.export([local, direct, network], home, @base_request)

    assert direct_reply["entries"] ==
             Enum.sort_by([direct.record, network.record], & &1["public_key"])

    onward_request = @base_request |> Directory.originate(first_peer) |> Directory.continue(home)
    onward_reply = Directory.export([local, direct, network], home, onward_request)

    assert onward_reply["entries"] == [network.record]
    assert onward_reply["routes"][network.record["public_key"]] == [hex(home)]
  end

  test "network replies bind the signed home relay to the returned route endpoint" do
    requester = key(10)
    direct_peer = key(11)
    provider_home = key(12)
    network = announcement(13, :network, provider_home)
    request = Directory.originate(@base_request, requester)

    good = %{
      "entries" => [network.record],
      "routes" => %{network.record["public_key"] => [hex(direct_peer), hex(provider_home)]},
      "next" => :null,
      "partial" => false
    }

    {page, imported} = Directory.combine([], [{direct_peer, {:ok, good}}], request)
    assert page["entries"] == [network.record]
    assert page["partial"] == false
    assert [{^direct_peer, %{relay_path: path}}] = imported
    assert path == [direct_peer, provider_home]

    wrong_endpoint =
      put_in(good, ["routes", network.record["public_key"]], [hex(direct_peer), hex(key(14))])

    {page, imported} = Directory.combine([], [{direct_peer, {:ok, wrong_endpoint}}], request)
    assert page["partial"]
    assert imported == []
  end

  test "rejects looped, forged, and oversized returned relay paths" do
    requester = key(20)
    peer = key(21)
    home = key(22)
    other = key(23)
    network = announcement(24, :network, home)
    request = Directory.originate(@base_request, requester)

    invalid_paths = [
      [other, peer, home],
      [peer, requester, home],
      [peer, other, peer, home],
      [peer, key(25), key(26), key(27), key(28), key(29), key(30), key(31), home]
    ]

    Enum.each(invalid_paths, fn path ->
      reply = reply(network, path)
      {page, imported} = Directory.combine([], [{peer, {:ok, reply}}], request)
      assert page["partial"]
      assert imported == []
    end)

    direct = announcement(32, :direct, home)

    {page, imported} =
      Directory.combine([], [{peer, {:ok, reply(direct, [peer, home])}}], request)

    assert page["partial"]
    assert imported == []
  end

  test "preserves downstream partial status and pages valid network results by public key" do
    requester = key(40)
    home = key(42)
    peer = home
    entries = Enum.map(43..45, &announcement(&1, :network, home))
    request = Directory.originate(Map.put(@base_request, "limit", 2), requester)
    remote = Directory.export(entries, home, request) |> Map.put("partial", true)

    {page, imported} = Directory.combine([], [{peer, {:ok, remote}}], request)
    expected = Enum.sort_by(entries, & &1.record["public_key"])

    assert page["entries"] == Enum.map(Enum.take(expected, 2), & &1.record)
    assert page["next"] == Enum.at(expected, 1).record["public_key"]
    assert page["partial"]
    assert length(imported) == 2

    following = Map.put(request, "after", page["next"])
    remote = Directory.export(entries, home, following)
    {page, _} = Directory.combine([], [{peer, {:ok, remote}}], following)
    assert page["entries"] == [List.last(expected).record]
    assert page["next"] == :null
  end

  test "binds network ingress to the authenticated preceding peer and validates hop budget" do
    previous = key(50)
    home = key(51)
    request = Directory.originate(@base_request, previous)

    assert Directory.valid_ingress?(request, previous, home)
    refute Directory.valid_ingress?(request, key(52), home)

    revisiting_home = Directory.continue(request, home)
    refute Directory.valid_ingress?(revisiting_home, home, home)

    refute Directory.can_continue?(put_in(request, ["network", "budget"], 1))

    hop_bound =
      put_in(request, ["network", "path"], Enum.map(53..60, &hex(key(&1))))

    refute Directory.can_continue?(hop_bound)
    refute Directory.valid_request?(put_in(request, ["network", "budget"], 65))
    refute Directory.valid_request?(put_in(request, ["network", "budget"], 0))
  end

  test "splits the downstream budget and reports unselected peers" do
    peers = [key(70), key(71), key(72)] |> Enum.sort()
    {:ok, manager} = start_fake_manager(self())
    on_exit(fn -> if Process.alive?(manager), do: GenServer.stop(manager, :normal) end)

    request = @base_request |> Directory.originate(key(73)) |> put_in(["network", "budget"], 4)
    responses = Directory.query(manager, peers, request)

    assert Enum.map(responses, &elem(&1, 0)) == peers

    for peer <- peers do
      assert_receive {:federation_request, ^peer, branch, _timeout}
      assert branch["network"]["budget"] == 1
    end

    request = put_in(request, ["network", "budget"], 2)
    responses = Directory.query(manager, peers, request)
    [selected | omitted] = peers

    assert_receive {:federation_request, ^selected, branch, _timeout}
    assert branch["network"]["budget"] == 1
    refute_receive {:federation_request, _, _, _}
    assert Enum.map(responses, &elem(&1, 0)) == peers
    assert Enum.at(responses, 0) == {selected, {:ok, empty_reply()}}

    assert Enum.map(Enum.drop(responses, 1), &elem(&1, 1)) ==
             Enum.map(omitted, fn _ -> {:error, :federation_budget_exhausted} end)
  end

  defp announcement(seed, :local, _home) do
    record = RelayAnnouncement.create(identity(seed), [%{"id" => "files"}])
    {:ok, entry} = RelayAnnouncement.verify(record)
    entry
  end

  defp announcement(seed, federation, home) when federation in [:direct, :network] do
    record =
      RelayAnnouncement.create(identity(seed), [%{"id" => "files"}],
        federation: federation,
        relay_public_key: home
      )

    {:ok, entry} = RelayAnnouncement.verify(record)
    entry
  end

  defp reply(entry, path) do
    %{
      "entries" => [entry.record],
      "routes" => %{entry.record["public_key"] => Enum.map(path, &hex/1)},
      "next" => :null,
      "partial" => false
    }
  end

  defp empty_reply, do: %{"entries" => [], "routes" => %{}, "next" => :null, "partial" => false}

  defp start_fake_manager(parent) do
    GenServer.start_link(__MODULE__.FakeManager, parent)
  end

  defp identity(seed), do: Identity.from_seed(:binary.copy(<<seed>>, 32))
  defp key(seed), do: identity(seed).public_key
  defp hex(key), do: Base.encode16(key, case: :lower)

  defmodule FakeManager do
    use GenServer

    def init(parent), do: {:ok, parent}

    def handle_call({:request, peer, branch, timeout}, _from, parent) do
      send(parent, {:federation_request, peer, branch, timeout})
      {:reply, {:ok, empty_reply()}, parent}
    end

    defp empty_reply,
      do: %{"entries" => [], "routes" => %{}, "next" => :null, "partial" => false}
  end
end
