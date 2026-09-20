defmodule Arc.CLIDiscoveryTest do
  use ExUnit.Case, async: false

  alias Arc.Data.Agent
  alias Arc.Identity
  alias Arc.Identity.KeyStore

  setup do
    Arc.Control.Local.reset()
    System.delete_env("ARC_KEY")

    on_exit(fn -> System.delete_env("ARC_KEY") end)

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
      Arc.CLI.TestTeardown.stop(hello_server)
      Arc.CLI.TestTeardown.stop(sqlite_server)
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
