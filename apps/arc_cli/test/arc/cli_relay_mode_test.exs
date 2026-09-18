defmodule Arc.CLIRelayModeTest do
  use ExUnit.Case, async: false

  alias Arc.Data.Agent
  alias Arc.Data.CapabilityDiscovery
  alias Arc.Identity

  setup do
    :ok = Arc.Control.Local.reset()
    :ok = Arc.Net.TransportManager.reset()
    root = Path.join(System.tmp_dir!(), "arc-relay-mode-#{System.unique_integer([:positive])}")
    previous = Application.get_env(:arc_data, :mailbox_dir)
    Application.put_env(:arc_data, :mailbox_dir, root)
    {:ok, relay} = Arc.Net.Relay.start_link(0)

    on_exit(fn ->
      Arc.Net.TransportManager.reset()
      Arc.CLI.TestTeardown.stop(relay)
      Application.put_env(:arc_data, :mailbox_dir, previous)
      File.rm_rf!(root)
      Arc.Control.Local.reset()
    end)

    %{
      relay: relay,
      port: Arc.Net.Relay.get_port(relay),
      pin: Arc.Net.Relay.get_pubkey(relay),
      root: root
    }
  end

  test "relay discovery and detail bypass shared local state and fail closed on disconnect",
       ctx do
    client_id = Identity.generate()
    provider_id = Identity.generate()
    local_only_id = Identity.generate()
    runtime = Path.expand("../../../../test/fixtures/providers/hello-provider.exs", __DIR__)
    manifest = Path.expand("../../../../test/fixtures/providers/hello-provider.json", __DIR__)
    {:ok, client} = Agent.start_link(client_id)

    {:ok, provider} =
      Agent.start_link(provider_id,
        serve: "exec://#{runtime}?manifest=#{URI.encode_www_form(manifest)}"
      )

    {:ok, local_only} = Agent.start_link(local_only_id)
    :ok = Agent.publish(local_only)

    on_exit(fn ->
      Enum.each([client, provider, local_only], &Arc.CLI.TestTeardown.stop/1)
    end)

    for {identity, agent} <- [{client_id, client}, {provider_id, provider}] do
      assert :ok = Arc.Net.connect_relay(~c"127.0.0.1", ctx.port, identity, ctx.pin)
      assert :ok = Agent.publish_relay(agent)
    end

    assert {:ok, []} = Arc.Control.resolve(Identity.name(provider_id))
    assert {:ok, %{matches: [%{provider: found}]}} = CapabilityDiscovery.discover(client, "hello")
    assert found["public_key"] == Identity.encode_public_key(provider_id)
    assert {:error, :not_found} = Agent.connect(client, Identity.name(local_only_id))

    handler_id = {__MODULE__, self()}

    :ok =
      :telemetry.attach(
        handler_id,
        [:arc, :net, :relay, :packet, :forwarded],
        &__MODULE__.forwarded/4,
        self()
      )

    on_exit(fn -> :telemetry.detach(handler_id) end)

    assert {:ok, detail} =
             CapabilityDiscovery.fetch_detail(client, Identity.name(provider_id), "primary")

    assert {:ok, _} = Arc.Data.CapabilityPackage.verify(detail)
    assert_receive :forwarded
    assert_receive :forwarded

    # These agents remain in the same VM. Losing the transport must not let
    # the local registry or a local control entry satisfy the request.
    :ok = Arc.Net.TransportManager.reset()

    assert {:error, :relay_not_connected} =
             Agent.send_message(client, Identity.name(provider_id), "must not bypass relay")

    assert {:error, :relay_not_connected} = CapabilityDiscovery.discover(client, "hello")
    assert Agent.read_inbox(provider) == []
    assert Path.wildcard(Path.join(ctx.root, "**/*")) == []
  end

  def forwarded(_event, _measurements, _metadata, parent), do: send(parent, :forwarded)

  test "a signed unusable peer key fails without killing the client", ctx do
    identity = Identity.generate()
    peer = Identity.generate()
    {:ok, client} = Agent.start_link(identity)
    on_exit(fn -> Arc.CLI.TestTeardown.stop(client) end)

    for id <- [identity, peer] do
      assert :ok = Arc.Net.connect_relay(~c"127.0.0.1", ctx.port, id, ctx.pin)
    end

    assert :ok = Agent.publish_relay(client)

    unsigned =
      Arc.Data.RelayAnnouncement.create(peer, [])
      |> Map.delete("signature")
      |> Map.put("x25519_public", String.duplicate("0", 64))

    # All fields here are scalars or the empty capability list. Independently
    # encode the public format so this remains a valid signature test.
    body =
      unsigned
      |> Enum.sort()
      |> Enum.map(fn {key, value} -> [json(key), ":", json(value)] end)
      |> Enum.intersperse(",")
      |> then(&IO.iodata_to_binary(["{", &1, "}"]))

    signature =
      Identity.sign(peer, "arc-relay-announcement-v1\n" <> body) |> Base.encode16(case: :lower)

    record = Map.put(unsigned, "signature", signature)
    assert :ok = Arc.Net.announce(peer.public_key, record)
    assert {:error, :invalid_peer_key} = Agent.connect(client, Identity.name(peer))
    assert Process.alive?(client)
  end

  test "direct federation opt-in binds the live announcement to its relay", ctx do
    identity = Identity.generate()
    {:ok, agent} = Agent.start_link(identity)
    on_exit(fn -> Arc.CLI.TestTeardown.stop(agent) end)

    assert :ok = Arc.Net.connect_relay(~c"127.0.0.1", ctx.port, identity, ctx.pin)
    assert :ok = Agent.publish_relay(agent, federation: :direct)

    assert {:ok, [entry]} =
             Arc.Net.resolve_via_relay(identity.public_key, Identity.name(identity))

    assert entry.federation == :direct
    assert entry.relay_public_key == ctx.pin
    assert Arc.Data.RelayAnnouncement.federatable?(entry, ctx.pin)
  end

  test "network federation opt-in keeps its onward scope on the live announcement", ctx do
    identity = Identity.generate()
    {:ok, agent} = Agent.start_link(identity)
    on_exit(fn -> Arc.CLI.TestTeardown.stop(agent) end)

    assert :ok = Arc.Net.connect_relay(~c"127.0.0.1", ctx.port, identity, ctx.pin)
    assert :ok = Agent.publish_relay(agent, federation: :network)

    assert {:ok, [entry]} =
             Arc.Net.resolve_via_relay(identity.public_key, Identity.name(identity))

    assert entry.federation == :network
    assert entry.relay_public_key == ctx.pin
    assert Arc.Data.RelayAnnouncement.federatable?(entry, ctx.pin)
    assert Agent.info(agent).relay_federation == :network
  end

  defp json(value), do: value |> :json.encode() |> IO.iodata_to_binary()
end
