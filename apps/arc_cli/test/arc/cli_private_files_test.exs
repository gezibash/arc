defmodule Arc.CLI.PrivateFilesIntegrationTest do
  use ExUnit.Case, async: false

  alias Arc.CLI.ProviderBundle
  alias Arc.Data.Agent
  alias Arc.Identity
  alias Arc.Identity.KeyStore

  setup do
    root = Path.join(System.tmp_dir!(), "arc-private-files-#{System.unique_integer([:positive])}")
    keys_dir = Path.join(root, "keys")
    tool_dir = Path.join(root, "tools")
    trust_dir = Path.join(root, "trust")
    control_dir = Path.join(root, "control")
    files_root = Path.join(root, "provider-storage")

    previous = %{
      keys_dir: Application.get_env(:arc_identity, :keys_dir),
      default_file: Application.get_env(:arc_identity, :default_file),
      tool_dir: Application.get_env(:arc_cli, :tool_registry_dir),
      trust_dir: Application.get_env(:arc_cli, :trust_store_dir),
      control_dir: Application.get_env(:arc_control, :control_dir),
      arc_key: System.get_env("ARC_KEY"),
      files_root: System.get_env("FILES_ROOT"),
      files_quota_bytes: System.get_env("FILES_QUOTA_BYTES")
    }

    Application.put_env(:arc_identity, :keys_dir, keys_dir)
    Application.put_env(:arc_identity, :default_file, Path.join(root, "default_key"))
    Application.put_env(:arc_cli, :tool_registry_dir, tool_dir)
    Application.put_env(:arc_cli, :trust_store_dir, trust_dir)
    Application.put_env(:arc_control, :control_dir, control_dir)
    System.put_env("FILES_ROOT", files_root)
    System.put_env("FILES_QUOTA_BYTES", Integer.to_string(16 * 1024 * 1024))
    System.delete_env("ARC_KEY")
    :ok = Arc.Control.Local.reset()

    on_exit(fn ->
      :ok = Arc.Control.Local.reset()
      restore_application_env(:arc_identity, :keys_dir, previous.keys_dir)
      restore_application_env(:arc_identity, :default_file, previous.default_file)
      restore_application_env(:arc_cli, :tool_registry_dir, previous.tool_dir)
      restore_application_env(:arc_cli, :trust_store_dir, previous.trust_dir)
      restore_application_env(:arc_control, :control_dir, previous.control_dir)
      restore_system_env("ARC_KEY", previous.arc_key)
      restore_system_env("FILES_ROOT", previous.files_root)
      restore_system_env("FILES_QUOTA_BYTES", previous.files_quota_bytes)
      File.rm_rf!(root)
    end)

    %{root: root, files_root: files_root}
  end

  test "private files install, store, list, retrieve, and isolate citizens", ctx do
    _alice = save_active_identity()
    bob = Identity.generate()
    :ok = KeyStore.save(bob)
    provider_id = Identity.generate()
    provider_root = Path.expand("../../../../providers/files", __DIR__)

    assert {:ok, serve_uri, _bundle} = ProviderBundle.resolve_serve_target(provider_root)
    assert {:ok, provider} = Agent.start_link(provider_id, serve: serve_uri)
    :ok = Agent.publish(provider)

    on_exit(fn ->
      Arc.CLI.TestTeardown.stop(provider)
    end)

    install_output =
      ExUnit.CaptureIO.capture_io("y\n", fn ->
        Arc.CLI.main(["install", Identity.name(provider_id), "primary"])
      end)

    assert install_output =~ "Installed files"

    name = "citizen-only-#{System.unique_integer([:positive])}.bin"
    source = Path.join(ctx.root, name)
    bytes = <<0, 255, 128, 10, 13>> <> "private content #{System.unique_integer([:positive])}"
    File.write!(source, bytes)

    put_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["files", "put", source])
      end)

    assert [id] = Regex.run(~r/\b[a-f0-9]{64}\b/, put_output)
    refute put_output =~ name
    refute put_output =~ bytes

    list_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["files", "list"])
      end)

    assert list_output =~ id
    assert list_output =~ name
    refute list_output =~ bytes
    refute plaintext_present?(ctx.files_root, name)
    refute plaintext_present?(ctx.files_root, bytes)

    destination = Path.join(ctx.root, "restored.bin")

    get_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["files", "get", id, "--output", destination])
      end)

    assert get_output =~ "Saved #{id}"
    assert File.read!(destination) == bytes

    {_result, overwrite_error} =
      ExUnit.CaptureIO.with_io(:stderr, fn ->
        assert {:exit, 1} = Arc.CLI.main(["files", "get", id, "--output", destination])
      end)

    assert overwrite_error =~ "output already exists"
    assert File.read!(destination) == bytes

    System.put_env("ARC_KEY", Identity.name(bob))

    ExUnit.CaptureIO.capture_io("y\n", fn ->
      Arc.CLI.main(["install", Identity.name(provider_id), "primary"])
    end)

    stranger_destination = Path.join(ctx.root, "stranger.bin")

    {_result, stranger_error} =
      ExUnit.CaptureIO.with_io(:stderr, fn ->
        assert {:exit, 1} =
                 Arc.CLI.main(["files", "get", id, "--output", stranger_destination])
      end)

    assert stranger_error =~ "not_found"
    refute File.exists?(stranger_destination)
  end

  test "an exactly 4 MiB binary file survives the real provider wire round trip", ctx do
    _citizen = save_active_identity()
    provider_id = Identity.generate()
    provider_root = Path.expand("../../../../providers/files", __DIR__)
    source = Path.join(ctx.root, "at-limit.bin")
    destination = Path.join(ctx.root, "at-limit-restored.bin")
    bytes = :binary.copy(<<0, 255, 128, 13, 10, 65, 66, 67>>, 512 * 1024)

    assert byte_size(bytes) == 4 * 1024 * 1024
    File.write!(source, bytes)

    assert {:ok, serve_uri, _bundle} = ProviderBundle.resolve_serve_target(provider_root)
    assert {:ok, provider} = Agent.start_link(provider_id, serve: serve_uri)
    :ok = Agent.publish(provider)

    on_exit(fn ->
      Arc.CLI.TestTeardown.stop(provider)
    end)

    ExUnit.CaptureIO.capture_io("y\n", fn ->
      Arc.CLI.main(["install", Identity.name(provider_id), "primary"])
    end)

    put_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["files", "put", source])
      end)

    assert [id] = Regex.run(~r/\b[a-f0-9]{64}\b/, put_output)

    ExUnit.CaptureIO.capture_io(fn ->
      Arc.CLI.main(["files", "get", id, "--output", destination])
    end)

    assert File.read!(destination) == bytes
  end

  defp save_active_identity do
    identity = Identity.generate()
    :ok = KeyStore.save(identity)
    System.put_env("ARC_KEY", Identity.name(identity))
    identity
  end

  defp plaintext_present?(root, value) do
    root
    |> Path.join("**/*")
    |> Path.wildcard()
    |> Enum.any?(fn path ->
      :binary.match(path, value) != :nomatch or
        (File.regular?(path) and :binary.match(File.read!(path), value) != :nomatch)
    end)
  end

  defp restore_application_env(app, key, nil), do: Application.delete_env(app, key)
  defp restore_application_env(app, key, value), do: Application.put_env(app, key, value)
  defp restore_system_env(key, nil), do: System.delete_env(key)
  defp restore_system_env(key, value), do: System.put_env(key, value)
end
