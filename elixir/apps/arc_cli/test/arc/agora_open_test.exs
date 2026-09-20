defmodule Arc.CLI.AgoraOpenTest do
  use ExUnit.Case, async: false

  alias Arc.CLI.{AgoraOpen, AgoraWeb, ToolRegistry}
  alias Arc.Data.CapabilityPackage
  alias Arc.Identity
  alias Arc.Identity.KeyStore

  @manifest Path.expand("../../../../providers/agora/manifest.json", __DIR__)

  setup do
    root = Path.join(System.tmp_dir!(), "agora-open-#{System.unique_integer([:positive])}")

    for {app, key, path} <- [
          {:arc_identity, :keys_dir, "keys"},
          {:arc_identity, :default_file, "default_key"},
          {:arc_cli, :tool_registry_dir, "tools"},
          {:arc_net, :relay_config_path, "relays.json"}
        ] do
      previous = Application.fetch_env(app, key)
      Application.put_env(app, key, Path.join(root, path))

      on_exit(fn ->
        case previous do
          {:ok, value} -> Application.put_env(app, key, value)
          :error -> Application.delete_env(app, key)
        end
      end)
    end

    for name <- ~w(ARC_KEY ARC_RELAY ARC_RELAY_PUBKEY) do
      previous = System.get_env(name)
      System.delete_env(name)

      on_exit(fn ->
        if previous, do: System.put_env(name, previous), else: System.delete_env(name)
      end)
    end

    on_exit(fn -> File.rm_rf!(root) end)
    :ok
  end

  test "rejects malformed launch options before loading an identity" do
    for args <- [[], ["agora", "extra"], ["agora", "--host", "0.0.0.0"], ["agora", "--port"]] do
      assert {:error, "usage:" <> _} = AgoraOpen.start(args)
    end

    assert {:error, "invalid --port" <> _} = AgoraOpen.start(["agora", "--port", "65536"])
    assert {:error, "invalid relay address" <> _} = AgoraOpen.start(["agora", "--relay", "bad"])

    assert {:error, "invalid relay public key" <> _} =
             AgoraOpen.start(["agora", "--relay-pubkey", "bad"])
  end

  test "invalid relay environment fails rather than switching to local mode" do
    System.put_env("ARC_RELAY", "invalid")
    assert {:error, "invalid relay address" <> _} = AgoraOpen.start(["agora"])
    System.put_env("ARC_RELAY", "127.0.0.1:9000")
    System.put_env("ARC_RELAY_PUBKEY", "invalid")
    assert {:error, "invalid relay public key" <> _} = AgoraOpen.start(["agora"])
  end

  test "requires an active identity and its own installed board" do
    assert {:error, "no active identity" <> _} = AgoraOpen.start(["agora"])
    activate_identity()
    assert {:error, "this command is not installed" <> _} = AgoraOpen.start(["agora"])
  end

  test "opens an installed alias and keeps installations scoped to the active citizen" do
    owner = activate_identity()
    {:ok, package} = CapabilityPackage.load_file(@manifest)
    signed = CapabilityPackage.sign(Identity.generate(), package)
    {:ok, _tool} = ToolRegistry.install(owner, signed, command: "square")

    assert {:ok, server} = AgoraOpen.start(["square", "--port", "0"])
    assert AgoraWeb.url(server) =~ ~r{^http://127\.0\.0\.1:\d+/$}
    GenServer.stop(server, :normal)

    activate_identity()
    assert {:error, "this command is not installed" <> _} = AgoraOpen.start(["square"])
  end

  test "does not treat an ordinary installed capability as a browser app" do
    owner = activate_identity()
    fixture = Path.expand("../../../../test/fixtures/providers/hello-provider.json", __DIR__)
    {:ok, package} = CapabilityPackage.load_file(fixture)
    signed = CapabilityPackage.sign(Identity.generate(), package)
    {:ok, _tool} = ToolRegistry.install(owner, signed, command: "agora")

    assert {:error, "the installed command does not expose an Agora interface"} =
             AgoraOpen.start(["agora"])
  end

  defp activate_identity do
    {:ok, identity} = KeyStore.generate()
    :ok = KeyStore.set_default(Identity.name(identity))
    identity
  end
end
