defmodule Arc.CLI.UpdateTest do
  use ExUnit.Case, async: false
  import ExUnit.CaptureIO

  alias Arc.CLI.Update.{Engine, Installer, Manifest}
  alias Arc.Data.Agent
  alias Arc.Identity
  alias Arc.Identity.KeyStore
  alias Arc.Net.RelayConfig

  @bundle Path.expand("../../../../providers/releases", __DIR__)
  @plan String.duplicate("b", 64)

  setup do
    :ok = Arc.Control.Local.reset()
    :ok = Arc.Net.TransportManager.reset()

    root = Path.join(System.tmp_dir!(), "arc-cli-update-#{System.unique_integer([:positive])}")
    releases = Path.join(root, "releases")
    install_root = Path.join([root, "share", "arc"])
    File.mkdir_p!(Path.join(releases, "channels"))
    File.mkdir_p!(Path.join(releases, "blobs"))
    build_release(install_root, "0.4.1")

    previous_env =
      for name <- ["RELEASES_ROOT", "RELEASE_ROOT", "ARC_RELAY", "ARC_RELAY_PUBKEY", "ARC_KEY"],
          into: %{},
          do: {name, System.get_env(name)}

    System.put_env("RELEASES_ROOT", releases)
    System.put_env("RELEASE_ROOT", install_root)
    System.delete_env("ARC_RELAY")
    System.delete_env("ARC_RELAY_PUBKEY")
    System.delete_env("ARC_KEY")

    previous_config =
      for {app, key} <- [
            {:arc_net, :relay_config_path},
            {:arc_cli, :update_state_dir},
            {:arc_identity, :keys_dir},
            {:arc_identity, :default_file}
          ],
          into: %{},
          do: {{app, key}, Application.fetch_env(app, key)}

    Application.put_env(:arc_net, :relay_config_path, Path.join(root, "relays.json"))
    Application.put_env(:arc_cli, :update_state_dir, Path.join(root, "update-state"))
    Application.put_env(:arc_identity, :keys_dir, Path.join(root, "keys"))
    Application.put_env(:arc_identity, :default_file, Path.join(root, "default.key"))

    # The updater runs as the selected citizen, the key `arc join` would create.
    {:ok, citizen} = KeyStore.generate()
    :ok = KeyStore.set_default(Identity.name(citizen))

    {:ok, relay} = Arc.Net.Relay.start_link(0)
    port = Arc.Net.Relay.get_port(relay)
    pin = Base.encode16(Arc.Net.Relay.get_pubkey(relay), case: :lower)
    :ok = RelayConfig.remember("127.0.0.1:#{port}", pin)

    provider_identity = Identity.generate()

    {:ok, provider} =
      Agent.start_link(provider_identity,
        serve:
          "exec://#{Path.join(@bundle, "run.sh")}?manifest=#{URI.encode_www_form(Path.join(@bundle, "manifest.json"))}"
      )

    :ok =
      Arc.Net.connect_relay(
        ~c"127.0.0.1",
        port,
        provider_identity,
        Arc.Net.Relay.get_pubkey(relay)
      )

    :ok = Agent.publish_relay(provider)

    on_exit(fn ->
      Arc.CLI.TestTeardown.stop(provider)
      Arc.CLI.TestTeardown.stop(relay)

      for {name, value} <- previous_env do
        if value, do: System.put_env(name, value), else: System.delete_env(name)
      end

      for {{app, key}, previous} <- previous_config do
        case previous do
          {:ok, value} -> Application.put_env(app, key, value)
          :error -> Application.delete_env(app, key)
        end
      end

      Arc.Net.TransportManager.reset()
      Arc.Control.Local.reset()
      File.rm_rf!(root)
    end)

    %{
      root: root,
      releases: releases,
      install_root: install_root,
      port: port,
      provider: provider_identity,
      citizen: citizen,
      publisher: Identity.generate()
    }
  end

  test "arc update finds the relay's release provider, verifies, and replaces the install", ctx do
    archive = build_archive(Path.join(ctx.root, "work"), "9.9.9")
    publish_channel(ctx, archive)
    publisher = Identity.encode_public_key(ctx.publisher)

    {result, output} = run(["update", "--publisher", publisher, "--format", "json"])
    assert result == :ok, output
    document = :json.decode(output)

    assert document["state"] == "current"
    assert document["installed"]["version"] == "0.4.1"
    assert document["install"]["version"] == "9.9.9"
    assert document["latest"]["version"] == "9.9.9"

    assert document["source"] ==
             "releases+arc://#{Identity.encode_public_key(ctx.provider)}/releases"

    assert document["relay"] == "127.0.0.1:#{ctx.port}"
    assert document["identity"] == Identity.name(ctx.citizen)
    assert document["publisher"] == publisher
    assert document["previous"] == ctx.install_root <> ".previous"

    assert {:ok, "9.9.9"} = Installer.installed_version(ctx.install_root)
    assert {:ok, "0.4.1"} = Installer.installed_version(ctx.install_root <> ".previous")

    assert {"arc 9.9.9 (test)\n", 0} =
             System.cmd(Path.join([ctx.install_root, "bin", "arc"]), ["version"])

    refute File.exists?(
             Path.join([ctx.root, "update-state", "staging", archive.sha256 <> ".tar.gz"])
           )

    # The trusted publisher and the checkpoint persist for the next run.
    {result, output} = run(["update", "check", "--json"])
    assert result == :ok, output
    check = :json.decode(output)
    assert check["state"] == "up_to_date"
    assert check["installed"]["version"] == "9.9.9"
    assert check["publisher"] == publisher

    {result, output} = run(["update", "status"])
    assert result == :ok, output
    assert output =~ "current"
    assert output =~ ctx.install_root

    # A different publisher is refused until the replacement is explicit.
    other = Identity.encode_public_key(Identity.generate())
    {result, stderr} = run_stderr(["update", "check", "--publisher", other])
    assert result == {:exit, 1}
    assert stderr =~ "--replace-publisher"
  end

  test "arc update installs a schema-two restart-only release from the publisher", ctx do
    archive = build_archive(Path.join(ctx.root, "work"), "9.9.9")
    File.cp!(archive.path, Path.join([ctx.releases, "blobs", archive.sha256 <> ".tar.gz"]))

    unsigned = %{
      "schema_version" => 2,
      "channel" => "stable",
      "publisher" => Identity.encode_public_key(ctx.publisher),
      "sequence" => 1,
      "expires_at" => System.system_time(:second) + 3_600,
      "releases" => [
        %{
          "version" => "9.9.9",
          "build" => "build-999",
          "runtime" => "16.0",
          "platform" => Engine.platform(),
          "size" => archive.size,
          "sha256" => archive.sha256,
          "sources" => [],
          "restart_required" => true,
          "withdrawn" => false,
          "eligible" => true,
          "install" => %{"sha256" => archive.sha256, "size" => archive.size}
        }
      ]
    }

    assert :ok = Arc.CLI.Update.Publisher.publish(ctx.releases, ctx.publisher, unsigned)

    publisher = Identity.encode_public_key(ctx.publisher)
    {result, output} = run(["update", "--publisher", publisher, "--format", "json"])
    assert result == :ok, output
    assert :json.decode(output)["install"]["version"] == "9.9.9"
    assert {:ok, "9.9.9"} = Installer.installed_version(ctx.install_root)
  end

  test "arc update check reports availability without downloading or installing", ctx do
    archive = build_archive(Path.join(ctx.root, "work"), "9.9.9")
    publish_channel(ctx, archive)
    publisher = Identity.encode_public_key(ctx.publisher)

    {result, output} = run(["update", "check", "--publisher", publisher])
    assert result == :ok, output
    assert output =~ "available"
    assert output =~ "9.9.9"
    assert {:ok, "0.4.1"} = Installer.installed_version(ctx.install_root)
    refute File.exists?(ctx.install_root <> ".previous")
    refute File.dir?(Path.join([ctx.root, "update-state", "staging"]))
  end

  test "arc update refuses a channel signed by another publisher", ctx do
    archive = build_archive(Path.join(ctx.root, "work"), "9.9.9")
    publish_channel(ctx, archive)
    other = Identity.encode_public_key(Identity.generate())

    {result, stderr} = run_stderr(["update", "--publisher", other])
    assert result == {:exit, 1}
    assert stderr =~ "publisher_mismatch"
    assert {:ok, "0.4.1"} = Installer.installed_version(ctx.install_root)
  end

  test "arc update needs a trusted publisher, a citizen key, and a release installation",
       ctx do
    {result, stderr} = run_stderr(["update", "check"])
    assert result == {:exit, 1}
    assert stderr =~ "--publisher"

    :ok = KeyStore.remove(Identity.name(ctx.citizen))
    {result, stderr} = run_stderr(["update", "check", "--publisher", String.duplicate("a", 64)])
    assert result == {:exit, 1}
    assert stderr =~ "arc keys"

    System.delete_env("RELEASE_ROOT")
    {result, stderr} = run_stderr(["update", "check", "--publisher", String.duplicate("a", 64)])
    assert result == {:exit, 1}
    assert stderr =~ "RELEASE_ROOT"
  end

  defp run(args) do
    result = nil
    output = capture_io(fn -> send(self(), {:result, Arc.CLI.main(args)}) end)
    result = receive_result(result)
    {result, output}
  end

  defp run_stderr(args) do
    result = nil

    stderr =
      capture_io(:stderr, fn ->
        capture_io(fn -> send(self(), {:result, Arc.CLI.main(args)}) end)
      end)

    {receive_result(result), stderr}
  end

  defp receive_result(_) do
    receive do
      {:result, result} -> result
    after
      0 -> :no_result
    end
  end

  defp publish_channel(ctx, archive) do
    release = %{
      "version" => "9.9.9",
      "build" => "build-999",
      "runtime" => "16.0",
      "platform" => Engine.platform(),
      "size" => archive.size,
      "sha256" => archive.sha256,
      "sources" => [
        %{
          "build" => "build-041",
          "runtime" => "16.0",
          "upgrade_plan_sha256" => @plan,
          "downgrade_plan_sha256" => @plan
        }
      ],
      "restart_required" => true,
      "withdrawn" => false,
      "eligible" => true,
      "install" => %{"sha256" => archive.sha256, "size" => archive.size}
    }

    unsigned = %{
      "schema_version" => 1,
      "channel" => "stable",
      "publisher" => Identity.encode_public_key(ctx.publisher),
      "sequence" => 1,
      "expires_at" => System.system_time(:second) + 3_600,
      "releases" => [release]
    }

    {:ok, signed} = Manifest.sign(ctx.publisher, unsigned)
    File.write!(Path.join([ctx.releases, "channels", "stable.json"]), :json.encode(signed))
    File.cp!(archive.path, Path.join([ctx.releases, "blobs", archive.sha256 <> ".tar.gz"]))
  end

  defp build_release(dir, version) do
    File.mkdir_p!(Path.join(dir, "bin"))
    File.mkdir_p!(Path.join([dir, "releases", version]))
    script = Path.join([dir, "bin", "arc"])
    File.write!(script, "#!/bin/sh\necho \"arc #{version} (test)\"\n")
    File.chmod!(script, 0o755)
    File.write!(Path.join([dir, "bin", "arc_runtime"]), "#!/bin/sh\nexit 0\n")
    File.chmod!(Path.join([dir, "bin", "arc_runtime"]), 0o755)
    File.write!(Path.join([dir, "releases", "start_erl.data"]), "16.0 #{version}\n")
    File.write!(Path.join([dir, "releases", version, "arc_runtime.rel"]), "{release, test}.\n")
    dir
  end

  defp build_archive(work, version) do
    File.mkdir_p!(work)
    tree = build_release(Path.join(work, "arc"), version)
    path = Path.join(work, "release.tar.gz")

    :ok =
      :erl_tar.create(String.to_charlist(path), [{~c"arc", String.to_charlist(tree)}], [
        :compressed
      ])

    bytes = File.read!(path)

    %{
      path: path,
      size: byte_size(bytes),
      sha256: :crypto.hash(:sha256, bytes) |> Base.encode16(case: :lower)
    }
  end
end
