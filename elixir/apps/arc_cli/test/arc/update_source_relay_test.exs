defmodule Arc.CLI.Update.SourceRelayTest do
  use ExUnit.Case, async: false

  alias Arc.CLI.Update.Source
  alias Arc.Data.Agent
  alias Arc.Identity

  @bundle Path.expand("../../../../providers/releases", __DIR__)

  setup do
    :ok = Arc.Control.Local.reset()
    :ok = Arc.Net.TransportManager.reset()

    root =
      Path.join(
        System.tmp_dir!(),
        "arc-update-source-relay-#{System.unique_integer([:positive])}"
      )

    releases = Path.join(root, "releases")
    staging = Path.join(root, "staging")
    File.mkdir_p!(Path.join(releases, "channels"))
    File.mkdir_p!(Path.join(releases, "blobs"))
    File.mkdir_p!(staging)

    previous_root = System.get_env("RELEASES_ROOT")
    System.put_env("RELEASES_ROOT", releases)

    {:ok, relay} = Arc.Net.Relay.start_link(0)
    telemetry_id = {__MODULE__, System.unique_integer([:positive])}

    :ok =
      :telemetry.attach(
        telemetry_id,
        [:arc, :net, :relay, :packet, :forwarded],
        &__MODULE__.forwarded/4,
        self()
      )

    on_exit(fn ->
      :telemetry.detach(telemetry_id)
      restore_env("RELEASES_ROOT", previous_root)
      Arc.CLI.TestTeardown.stop(relay)
      Arc.Net.TransportManager.reset()
      Arc.Control.Local.reset()
      File.rm_rf!(root)
    end)

    %{
      releases: releases,
      staging: staging,
      relay: relay,
      port: Arc.Net.Relay.get_port(relay),
      pin: Arc.Net.Relay.get_pubkey(relay)
    }
  end

  test "reads and stages a release provider artifact only through a pinned relay", ctx do
    channel = "{\"channel\":\"stable\",\"sequence\":1}"
    bytes = :crypto.strong_rand_bytes(300_000)
    digest = :crypto.hash(:sha256, bytes) |> Base.encode16(case: :lower)
    File.write!(Path.join([ctx.releases, "channels", "stable.json"]), channel)
    File.write!(Path.join([ctx.releases, "blobs", digest <> ".tar.gz"]), bytes)

    citizen_identity = Identity.generate()
    provider_identity = Identity.generate()
    {:ok, citizen} = Agent.start_link(citizen_identity)

    {:ok, provider} =
      Agent.start_link(provider_identity,
        serve:
          "exec://#{Path.join(@bundle, "run.sh")}?manifest=#{URI.encode_www_form(Path.join(@bundle, "manifest.json"))}"
      )

    on_exit(fn ->
      Enum.each([citizen, provider], &Arc.CLI.TestTeardown.stop/1)
    end)

    for {identity, agent} <- [{citizen_identity, citizen}, {provider_identity, provider}] do
      assert :ok = Arc.Net.connect_relay(~c"127.0.0.1", ctx.port, identity, ctx.pin)
      assert :ok = Agent.publish_relay(agent)
    end

    address = "releases+arc://#{Identity.encode_public_key(provider_identity)}/releases"
    source = {:arc, citizen, address}

    assert {:ok, ^channel} = Source.channel(source, "stable")

    destination = Path.join(ctx.staging, digest <> ".tar.gz")

    assert {:ok, ^destination} =
             Source.stage_archive(source, digest, byte_size(bytes), destination)

    assert File.read!(destination) == bytes
    assert relay_forwarded_count() >= 12
  end

  def forwarded(_event, _measurements, _metadata, test_pid),
    do: send(test_pid, :relay_packet_forwarded)

  defp relay_forwarded_count(count \\ 0) do
    receive do
      :relay_packet_forwarded -> relay_forwarded_count(count + 1)
    after
      0 -> count
    end
  end

  defp restore_env(name, nil), do: System.delete_env(name)
  defp restore_env(name, value), do: System.put_env(name, value)
end
