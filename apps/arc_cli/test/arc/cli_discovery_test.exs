defmodule Arc.CLIDiscoveryTest do
  use ExUnit.Case, async: false

  alias Arc.Identity
  alias Arc.Identity.KeyStore
  alias Arc.Data.Agent

  setup do
    Arc.Control.Local.reset()
    System.delete_env("ARC_KEY")

    mount_dir =
      Path.join(System.tmp_dir!(), "arc_cli_mounts_#{System.unique_integer([:positive])}")

    old_mount_dir = Application.get_env(:arc_mcp, :dynamic_tool_registry_dir)
    Application.put_env(:arc_mcp, :dynamic_tool_registry_dir, mount_dir)

    on_exit(fn ->
      System.delete_env("ARC_KEY")

      if old_mount_dir do
        Application.put_env(:arc_mcp, :dynamic_tool_registry_dir, old_mount_dir)
      else
        Application.delete_env(:arc_mcp, :dynamic_tool_registry_dir)
      end

      File.rm_rf!(mount_dir)
    end)

    :ok
  end

  test "arc discover narrows by query against capability summaries" do
    client_id = persist_cli_identity()
    hello_id = Identity.generate()
    sqlite_id = Identity.generate()
    {hello_runtime, hello_manifest} = hello_provider_paths()
    {sqlite_runtime, sqlite_manifest} = sqlite_provider_paths()

    File.chmod!(hello_runtime, 0o755)
    File.chmod!(sqlite_runtime, 0o755)

    {:ok, hello_server} =
      Agent.start_link(
        hello_id,
        serve: "exec://#{hello_runtime}?manifest=#{URI.encode_www_form(hello_manifest)}"
      )

    {:ok, sqlite_server} =
      Agent.start_link(
        sqlite_id,
        serve: "exec://#{sqlite_runtime}?manifest=#{URI.encode_www_form(sqlite_manifest)}"
      )

    :ok = Agent.publish(hello_server)
    :ok = Agent.publish(sqlite_server)

    on_exit(fn ->
      if Process.alive?(hello_server), do: GenServer.stop(hello_server, :normal)
      if Process.alive?(sqlite_server), do: GenServer.stop(sqlite_server, :normal)
      KeyStore.remove(Identity.name(client_id))
    end)

    output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["discover", "sqlite"])
      end)

    assert output =~ "Query: sqlite"
    assert output =~ "#{Identity.name(sqlite_id)}/primary [data/sql]"
    refute output =~ "#{Identity.name(hello_id)}/primary [service/hello]"
  end

  test "arc mount add and ls manage an identity-scoped task working set" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = hello_provider_paths()

    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(client_id))
    end)

    add_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["mount", "demo", "add", Identity.name(server_id), "primary"])
      end)

    list_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["mount", "demo", "ls"])
      end)

    assert add_output =~ "Mounted demo: #{Identity.name(server_id)}/primary [service/hello]"
    assert list_output =~ "Owner: #{Identity.name(client_id)}"
    assert list_output =~ "Task: demo"
    assert list_output =~ "Mounted: 1"
    assert list_output =~ "#{Identity.name(server_id)}/primary [service/hello]"
    assert list_output =~ "Invocation: RAW /"
  end

  test "arc mount ls only shows mounts for the active identity" do
    first_client = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = hello_provider_paths()

    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    second_client = Identity.generate()
    :ok = KeyStore.save(second_client)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(first_client))
      KeyStore.remove(Identity.name(second_client))
    end)

    ExUnit.CaptureIO.capture_io(fn ->
      Arc.CLI.main(["mount", "demo", "add", Identity.name(server_id), "primary"])
    end)

    System.put_env("ARC_KEY", Identity.name(second_client))

    list_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["mount", "demo", "ls"])
      end)

    assert list_output =~ "Owner: #{Identity.name(second_client)}"
    assert list_output =~ "Mounted: 0"
    refute list_output =~ "#{Identity.name(server_id)}/primary"
  end

  test "arc mount call invokes the mounted capability instead of manual send" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = hello_provider_paths()

    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(client_id))
    end)

    ExUnit.CaptureIO.capture_io(fn ->
      Arc.CLI.main(["mount", "demo", "add", Identity.name(server_id), "primary"])
    end)

    call_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main([
          "mount",
          "demo",
          "call",
          Identity.name(server_id),
          "primary",
          "GET /"
        ])
      end)

    assert call_output =~ "provider hello"
  end

  defp persist_cli_identity do
    identity = Identity.generate()
    :ok = KeyStore.save(identity)
    System.put_env("ARC_KEY", Identity.name(identity))
    identity
  end

  defp hello_provider_paths do
    runtime = Path.expand("../../../../test/fixtures/providers/hello-provider.exs", __DIR__)
    manifest = Path.expand("../../../../test/fixtures/providers/hello-provider.json", __DIR__)
    {runtime, manifest}
  end

  defp sqlite_provider_paths do
    runtime = Path.expand("../../../../test/fixtures/providers/sqlite-provider.exs", __DIR__)
    manifest = Path.expand("../../../../test/fixtures/providers/sqlite-provider.json", __DIR__)
    {runtime, manifest}
  end
end
