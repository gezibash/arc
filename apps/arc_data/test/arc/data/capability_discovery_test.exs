defmodule Arc.Data.CapabilityDiscoveryTest do
  use ExUnit.Case, async: false

  alias Arc.Identity
  alias Arc.Data.Agent
  alias Arc.Data.CapabilityDiscovery

  setup do
    Arc.Control.Local.reset()
    :ok
  end

  test "discover filters by summary fields without expanding detail" do
    client_id = Identity.generate()
    hello_id = Identity.generate()
    sqlite_id = Identity.generate()
    {hello_runtime, hello_manifest} = hello_provider_paths()
    {sqlite_runtime, sqlite_manifest} = sqlite_provider_paths()

    File.chmod!(hello_runtime, 0o755)
    File.chmod!(sqlite_runtime, 0o755)

    {:ok, client} = Agent.start_link(client_id)

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

    :ok = Agent.publish(client)
    :ok = Agent.publish(hello_server)
    :ok = Agent.publish(sqlite_server)

    assert {:ok, result} = CapabilityDiscovery.discover(client, "sqlite")
    assert result.total == 1

    [%{provider: provider, capability: capability}] = result.matches
    assert provider["name"] == Identity.name(sqlite_id)
    assert capability["kind"] == "data"
    assert capability["scheme"] == "sql"
    refute Map.has_key?(capability, "invocation")
    refute Map.has_key?(capability, "config")
  end

  test "fetch_detail expands a selected capability" do
    client_id = Identity.generate()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = sqlite_provider_paths()

    {:ok, client} = Agent.start_link(client_id)
    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(client)
    :ok = Agent.publish(server)

    assert {:ok, detail} =
             CapabilityDiscovery.fetch_detail(client, Identity.name(server_id), "primary")

    assert detail["provider"]["name"] == Identity.name(server_id)
    assert detail["capability"]["kind"] == "data"
    assert detail["capability"]["config"]["database"] == "demo.sqlite3"
    assert detail["capability"]["invocation"]["method"] == "RAW"
  end

  test "discover applies a bounded result limit" do
    client_id = Identity.generate()
    first_id = Identity.generate()
    second_id = Identity.generate()
    third_id = Identity.generate()
    {runtime_path, manifest_path} = hello_provider_paths()

    {:ok, client} = Agent.start_link(client_id)
    File.chmod!(runtime_path, 0o755)

    {:ok, first} =
      Agent.start_link(
        first_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    {:ok, second} =
      Agent.start_link(
        second_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    {:ok, third} =
      Agent.start_link(
        third_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(client)
    :ok = Agent.publish(first)
    :ok = Agent.publish(second)
    :ok = Agent.publish(third)

    assert {:ok, result} = CapabilityDiscovery.discover(client, "hello", limit: 2)
    assert result.total == 3
    assert result.truncated?
    assert length(result.matches) == 2
  end

  defp hello_provider_paths do
    runtime = Path.expand("../../../../../test/fixtures/providers/hello-provider.exs", __DIR__)
    manifest = Path.expand("../../../../../test/fixtures/providers/hello-provider.json", __DIR__)
    {runtime, manifest}
  end

  defp sqlite_provider_paths do
    runtime = Path.expand("../../../../../test/fixtures/providers/sqlite-provider.exs", __DIR__)
    manifest = Path.expand("../../../../../test/fixtures/providers/sqlite-provider.json", __DIR__)
    {runtime, manifest}
  end
end
