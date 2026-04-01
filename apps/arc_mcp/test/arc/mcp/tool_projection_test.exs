defmodule Arc.MCP.ToolProjectionTest do
  use ExUnit.Case, async: false

  alias Arc.Data.Agent
  alias Arc.Identity
  alias Arc.MCP.DynamicToolRegistry
  alias Arc.MCP.ToolProjection

  setup do
    Arc.Control.Local.reset()

    dir =
      Path.join(
        System.tmp_dir!(),
        "arc_tool_projection_#{System.unique_integer([:positive, :monotonic])}"
      )

    File.mkdir_p!(dir)

    on_exit(fn ->
      File.rm_rf!(dir)
    end)

    %{dir: dir}
  end

  test "list includes the default send tool and only the owner's mounts", %{dir: dir} do
    owner = Identity.generate()
    other_owner = Identity.generate()

    assert {:ok, _} =
             DynamicToolRegistry.mount(owner, "demo", detail_doc("alpha", "primary"), dir: dir)

    assert {:ok, _} =
             DynamicToolRegistry.mount(
               other_owner,
               "demo",
               detail_doc("beta", "secondary"),
               dir: dir
             )

    assert {:ok, descriptors} =
             ToolProjection.list(owner.public_key, "demo", registry_opts: [dir: dir])

    tool_names = Enum.map(descriptors, & &1.tool["name"])

    assert "send" in tool_names
    assert "arc_alpha__primary" in tool_names
    refute "arc_beta__secondary" in tool_names
  end

  test "send tool delivers a DM from the session identity", _context do
    sender = Identity.generate()
    receiver = Identity.generate()

    {:ok, sender_agent} = Agent.start_link(sender)
    {:ok, receiver_agent} = Agent.start_link(receiver)
    :ok = Agent.publish(sender_agent)
    :ok = Agent.publish(receiver_agent)

    on_exit(fn ->
      if Process.alive?(sender_agent), do: GenServer.stop(sender_agent, :normal)
      if Process.alive?(receiver_agent), do: GenServer.stop(receiver_agent, :normal)
    end)

    assert {:ok, result} =
             ToolProjection.call(
               sender_agent,
               sender.public_key,
               "demo",
               "send",
               %{"to" => Identity.name(receiver), "message" => "hello over arc"}
             )

    assert result["isError"] == false
    assert get_in(result, ["content", Access.at(0), "text"]) =~ "Sent to"

    Agent.poll_mailbox(receiver_agent)
    Process.sleep(25)

    assert [%{from: from, text: "hello over arc"}] = Agent.read_inbox(receiver_agent)
    assert from == Identity.name(sender)
  end

  defp detail_doc(provider_name, capability_id) do
    %{
      "provider" => %{"name" => provider_name, "short_name" => provider_name},
      "capability" => %{
        "id" => capability_id,
        "kind" => "service",
        "scheme" => "http",
        "title" => "HTTP Proxy",
        "summary" => "Proxy HTTP requests",
        "invocation" => %{"method" => "RAW", "path" => "/"}
      }
    }
  end
end
