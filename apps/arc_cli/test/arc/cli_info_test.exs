defmodule Arc.CLIInfoTest do
  use ExUnit.Case, async: false

  alias Arc.Data.Agent
  alias Arc.Identity
  alias Arc.Identity.KeyStore

  setup do
    Arc.Control.Local.reset()
    System.delete_env("ARC_KEY")

    on_exit(fn ->
      System.delete_env("ARC_KEY")
    end)

    :ok
  end

  test "arc info shows capability summary for a remote peer" do
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
      Arc.CLI.TestTeardown.stop(server)
      KeyStore.remove(Identity.name(client_id))
    end)

    output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["info", Identity.name(server_id)])
      end)

    assert output =~ "Provider: #{Identity.name(server_id)}"
    assert output =~ "Capabilities: 1"
    assert output =~ "primary [service/hello]"
    assert output =~ "Hello Service"
    assert output =~ "Expand: arc info #{Identity.name(server_id)} primary"
  end

  test "arc info <peer> <capability> shows one detail view" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = sqlite_provider_paths()

    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    on_exit(fn ->
      Arc.CLI.TestTeardown.stop(server)
      KeyStore.remove(Identity.name(client_id))
    end)

    output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["info", Identity.name(server_id), "primary"])
      end)

    assert output =~ "Provider: #{Identity.name(server_id)}"
    assert output =~ "Capability: primary"
    assert output =~ "Type: data/sql"
    assert output =~ "Invocation: RAW /"
    assert output =~ "database: demo.sqlite3"
    assert output =~ "SELECT 1 AS n"
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
