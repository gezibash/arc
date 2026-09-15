defmodule Arc.Data.AgoraInvocationTest do
  use ExUnit.Case, async: false

  alias Arc.Data.{Agent, CapabilityInvocation}
  alias Arc.Identity

  @fixtures Path.expand("../../../../../test/fixtures/providers", __DIR__)

  setup do
    Arc.Control.Local.reset()
    :ok
  end

  test "an Agora call reaches the pinned key despite a substituted provider name" do
    owner = Identity.generate()
    board = Identity.generate()
    substitute = Identity.generate()
    {:ok, client} = Agent.start_link(owner)
    {:ok, provider} = Agent.start_link(board, serve: echo_uri())
    {:ok, impostor} = Agent.start_link(substitute, serve: echo_uri())

    for agent <- [client, provider, impostor], do: :ok = Agent.publish(agent)

    on_exit(fn ->
      for agent <- [client, provider, impostor] do
        if Process.alive?(agent), do: GenServer.stop(agent, :normal)
      end
    end)

    mount = mount(Identity.encode_public_key(board), Identity.name(substitute))
    assert {:ok, reply} = CapabilityInvocation.invoke(client, mount, "pinned request")
    assert reply.from == Identity.name(board)
    assert :json.decode(reply.text)["message"] == "pinned request"
    assert Agent.info(impostor).sessions == 0
  end

  test "a substituted directory entry is rejected before sending any request" do
    parent = self()
    board = Identity.generate() |> Identity.encode_public_key()
    substitute_key = Identity.generate().public_key
    agent = spawn_link(fn -> substituted_directory(parent, substitute_key) end)
    on_exit(fn -> send(agent, :stop) end)

    assert {:error, :provider_identity_mismatch} =
             CapabilityInvocation.invoke(agent, mount(board, "spoofed-name"), "must not send")

    assert_receive {:looked_up, ^board}
    refute_receive :sent_request
    send(agent, :stop)
  end

  test "missing and malformed Agora pins fail before connecting" do
    for key <- [nil, "", "abc", String.duplicate("z", 64)] do
      assert {:error, :invalid_provider_key} =
               CapabilityInvocation.invoke(self(), mount(key, "untrusted-name"), "must not send")
    end
  end

  defp substituted_directory(parent, substitute_key) do
    receive do
      {:"$gen_call", from, {:connect, query}} ->
        send(parent, {:looked_up, query})
        GenServer.reply(from, {:ok, %{public_key: substitute_key}})
        substituted_directory(parent, substitute_key)

      {:"$gen_call", from, {:send, _, _, _}} ->
        send(parent, :sent_request)
        GenServer.reply(from, :ok)
        substituted_directory(parent, substitute_key)

      :stop ->
        :ok
    end
  end

  defp mount(key, name) do
    %{
      "provider" => %{"name" => name, "public_key" => key},
      "capability" => %{
        "id" => "primary",
        "invocation" => %{"method" => "RAW", "path" => "/"},
        "interfaces" => %{
          "cli" => %{
            "version" => 4,
            "namespace" => "agora",
            "commands" => [
              %{"path" => ["read"], "input" => %{"source" => "agora", "operation" => "read"}}
            ]
          }
        }
      }
    }
  end

  defp echo_uri do
    "exec://#{Path.join(@fixtures, "context-provider.exs")}?" <>
      URI.encode_query(%{"manifest" => Path.join(@fixtures, "context-provider.json")})
  end
end
