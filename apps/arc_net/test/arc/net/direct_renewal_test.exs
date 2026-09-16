defmodule Arc.Net.DirectRenewalTest do
  use ExUnit.Case, async: false

  alias Arc.Data.{Agent, Direct, Protocol}
  alias Arc.Identity
  alias Arc.Net.{Relay, TransportManager}

  @fixtures Path.expand("../../../../../test/fixtures/providers", __DIR__)

  test "a stalled real relay directory lookup does not block an active direct request" do
    ctx = pair()
    assert {:ok, %{body: "warm"}} = Protocol.request(ctx.client, ctx.uri, "warm")
    route = Direct.route(Agent.direct(ctx.client), ctx.target)
    assert is_map(route)

    {:ok, transport} = TransportManager.lookup(ctx.client_identity.public_key)
    :ok = :sys.suspend(transport)
    on_exit(fn -> safe_resume(transport) end)

    send(Agent.direct(ctx.client), {:renew, route.generation, route.deadline})

    assert eventually(fn -> map_size(:sys.get_state(ctx.client).refresh_workers) == 1 end)
    assert {:ok, %{body: "during"}} = Protocol.request(ctx.client, ctx.uri, "during")
    assert Direct.admitted?(Agent.direct(ctx.client), route.generation)
  end

  test "a pending relay request prevents refresh session replacement" do
    ctx = pair()
    assert {:ok, %{body: "warm"}} = Protocol.request(ctx.client, ctx.uri, "warm")
    route = Direct.route(Agent.direct(ctx.client), ctx.target)
    assert is_map(route)

    {:ok, provider_transport} = TransportManager.lookup(ctx.provider_identity.public_key)
    :ok = :sys.suspend(provider_transport)
    on_exit(fn -> safe_resume(provider_transport) end)

    request_id = Arc.Data.Frame.new_request_id()
    original_session_id = :sys.get_state(ctx.client).sessions[ctx.target.key].session_id

    assert :ok =
             Agent.send_message(ctx.client, ctx.target.provider, "pending",
               request_id: request_id,
               track_reply: true,
               deadline_ms: System.monotonic_time(:millisecond) + 2_000,
               meta: %{"method" => "RAW", "path" => "/main", "capability_id" => "primary"}
             )

    assert eventually(fn ->
             Map.has_key?(:sys.get_state(ctx.client).request_routes, {ctx.target.key, request_id})
           end)

    send(Agent.direct(ctx.client), {:renew, route.generation, route.deadline})
    Process.sleep(100)
    assert map_size(:sys.get_state(ctx.client).refresh_workers) == 0
    assert :sys.get_state(ctx.client).sessions[ctx.target.key].session_id == original_session_id

    assert {:ok, %{body: "direct-still-works"}} =
             Protocol.request(ctx.client, ctx.uri, "direct-still-works")

    assert Direct.admitted?(Agent.direct(ctx.client), route.generation)

    safe_resume(provider_transport)
    assert await_reply(ctx.client, request_id)
  end

  defp pair do
    relay_key = Identity.generate().public_key
    {:ok, relay} = Relay.start_link(0, relay_public_key: relay_key)
    port = Relay.get_port(relay)
    client_identity = Identity.generate()
    provider_identity = Identity.generate()
    lease_ms = 8_000

    {:ok, client} =
      Agent.start_link(client_identity,
        direct_policy: [rule(provider_identity.public_key, false, lease_ms)]
      )

    runtime = Path.join(@fixtures, "binary-echo-provider.exs")
    manifest = Path.join(@fixtures, "binary-echo-provider.json")

    {:ok, provider} =
      Agent.start_link(provider_identity,
        direct_policy: [rule(client_identity.public_key, true, lease_ms)],
        serve: "exec://#{runtime}?#{URI.encode_query(%{"manifest" => manifest})}"
      )

    for {agent, identity} <- [{client, client_identity}, {provider, provider_identity}] do
      :ok = Arc.Net.acquire_relay(~c"localhost", port, identity, relay_key, owner_pid: agent)
      :ok = Agent.publish_relay(agent)
    end

    uri = "binary-echo+arc://#{Identity.encode_public_key(provider_identity)}/main"
    {:ok, target} = Protocol.parse(uri)

    on_exit(fn ->
      for {agent, identity} <- [{client, client_identity}, {provider, provider_identity}] do
        Arc.Net.release_relay(identity.public_key, agent)
        stop(agent)

        case TransportManager.lookup(identity.public_key) do
          {:ok, transport} ->
            assert :ok = DynamicSupervisor.terminate_child(Arc.Net.TransportSupervisor, transport)

          _ ->
            :ok
        end
      end

      stop(relay)
    end)

    %{
      client: client,
      provider: provider,
      client_identity: client_identity,
      provider_identity: provider_identity,
      uri: uri,
      target: target
    }
  end

  defp rule(peer, listen?, lease_ms) do
    rule = %{
      "peer" => Base.encode16(peer, case: :lower),
      "capability" => "primary",
      "scheme" => "binary-echo",
      "path" => "/main",
      "lease_ms" => lease_ms,
      "dial" => ["127.0.0.1"]
    }

    if listen?,
      do:
        Map.put(rule, "listen", %{"bind" => "127.0.0.1", "address" => "127.0.0.1", "port" => 0}),
      else: rule
  end

  defp safe_resume(pid) do
    :sys.resume(pid)
  catch
    :exit, _ -> :ok
  end

  defp await_reply(agent, request_id, timeout \\ 2_000) do
    eventually(
      fn ->
        Agent.take_inbox(agent, fn message ->
          message[:request_id] == request_id and message[:kind] in [:response, :error]
        end) != []
      end,
      timeout
    )
  end

  defp stop(pid) do
    if Process.alive?(pid), do: GenServer.stop(pid, :normal)
  catch
    :exit, _ -> :ok
  end

  defp eventually(fun, timeout \\ 2_000) do
    deadline = System.monotonic_time(:millisecond) + timeout
    wait(fun, deadline)
  end

  defp wait(fun, deadline) do
    cond do
      fun.() ->
        true

      System.monotonic_time(:millisecond) >= deadline ->
        false

      true ->
        Process.sleep(20)
        wait(fun, deadline)
    end
  end
end
