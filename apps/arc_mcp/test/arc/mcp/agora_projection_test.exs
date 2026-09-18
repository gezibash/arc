defmodule Arc.MCP.AgoraProjectionTest do
  use ExUnit.Case, async: false

  alias Arc.Data.Agent
  alias Arc.Identity
  alias Arc.MCP.DynamicToolRegistry
  alias Arc.MCP.ToolProjection

  @provider_bundle Path.expand("../../../../../providers/agora", __DIR__)

  setup do
    Arc.Control.Local.reset()

    root = Path.join(System.tmp_dir!(), "arc-mcp-agora-#{System.unique_integer([:positive])}")
    registry_dir = Path.join(root, "mounts")
    board_dir = Path.join(root, "board")
    previous_agora_root = System.get_env("AGORA_ROOT")
    File.mkdir_p!(root)
    System.put_env("AGORA_ROOT", board_dir)

    on_exit(fn ->
      Arc.Control.Local.reset()
      restore_system_env("AGORA_ROOT", previous_agora_root)
      File.rm_rf!(root)
    end)

    %{registry_dir: registry_dir}
  end

  test "mounted Agora tool signs with its owner and verifies posts through the provider", %{
    registry_dir: registry_dir
  } do
    provider_identity = Identity.generate()
    alice = Identity.generate()
    bob = Identity.generate()

    assert {:ok, provider} =
             Agent.start_link(provider_identity, serve: provider_uri(provider_identity))

    assert {:ok, alice_agent} = Agent.start_link(alice)
    assert {:ok, bob_agent} = Agent.start_link(bob)

    on_exit(fn ->
      for agent <- [bob_agent, alice_agent, provider] do
        Arc.MCP.TestTeardown.stop(agent)
      end
    end)

    for agent <- [provider, alice_agent, bob_agent], do: :ok = Agent.publish(agent)

    detail = detail_doc(provider_identity)
    assert {:ok, _} = DynamicToolRegistry.mount(alice, "conversation", detail, dir: registry_dir)
    assert {:ok, _} = DynamicToolRegistry.mount(bob, "conversation", detail, dir: registry_dir)

    assert {:ok, descriptors} =
             ToolProjection.list(alice.public_key, "conversation", dir: registry_dir)

    agora_tool = Enum.find(descriptors, &(&1.tool["name"] != "send"))
    assert %{"inputSchema" => %{"required" => ["argv"]}} = agora_tool.tool
    tool_name = agora_tool.tool["name"]

    assert {:ok, post_result} =
             ToolProjection.call(
               alice_agent,
               alice.public_key,
               "conversation",
               tool_name,
               %{"argv" => ["post", "MCP-authored post"]},
               registry_opts: [dir: registry_dir]
             )

    assert post_result["isError"] == false
    assert %{"post" => post} = post_result["structuredContent"]
    assert post["author"] == Identity.encode_public_key(alice)
    assert post["board"] == Identity.encode_public_key(provider_identity)

    assert {:ok, reply_result} =
             ToolProjection.call(
               bob_agent,
               bob.public_key,
               "conversation",
               tool_name,
               %{"argv" => ["reply", post["id"], "MCP-authored reply"]},
               registry_opts: [dir: registry_dir]
             )

    assert %{"post" => reply} = reply_result["structuredContent"]
    assert reply["author"] == Identity.encode_public_key(bob)
    assert reply["parent"] == post["id"]

    assert {:ok, read_result} =
             ToolProjection.call(
               bob_agent,
               bob.public_key,
               "conversation",
               tool_name,
               %{"argv" => ["read", post["id"]]},
               registry_opts: [dir: registry_dir]
             )

    assert %{"post" => ^post} = read_result["structuredContent"]

    assert {:error, {:tool_error, error}} =
             ToolProjection.call(
               alice_agent,
               bob.public_key,
               "conversation",
               tool_name,
               %{"argv" => ["post", "wrong authenticated owner"]},
               registry_opts: [dir: registry_dir]
             )

    assert error =~ "Agora signer must match the authenticated owner"
  end

  defp detail_doc(provider_identity) do
    manifest = @provider_bundle |> Path.join("manifest.json") |> File.read!() |> :json.decode()

    %{
      "provider" => %{
        "name" => Identity.name(provider_identity),
        "short_name" => Identity.short_name(provider_identity),
        "public_key" => Identity.encode_public_key(provider_identity)
      },
      "capability" =>
        manifest["capability"]
        |> Map.put("interfaces", manifest["interfaces"])
    }
  end

  defp provider_uri(provider_identity) do
    runtime = Path.join(@provider_bundle, "run.sh")
    manifest = Path.join(@provider_bundle, "manifest.json")

    "exec://#{runtime}?" <>
      URI.encode_query(%{
        "manifest" => manifest,
        "arc_public_key" => Identity.encode_public_key(provider_identity)
      })
  end

  defp restore_system_env(key, nil), do: System.delete_env(key)
  defp restore_system_env(key, value), do: System.put_env(key, value)
end
