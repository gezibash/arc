defmodule Arc.Net.DirectPromotionTest do
  use ExUnit.Case, async: false

  alias Arc.Data.{Agent, Direct, Protocol}
  alias Arc.Identity
  alias Arc.Net.{Relay, TransportManager}

  @fixtures Path.expand("../../../../../test/fixtures/providers", __DIR__)

  test "promotes through a relay and keeps exact bytes flowing during an outage until expiry" do
    ctx = pair(:provider, lease_ms: 4_000)
    body = :binary.copy(<<0, 255, 128, 10, 65>>, 1024)
    assert {:ok, %{body: ^body}} = Protocol.request(ctx.client, ctx.uri, body)
    route = await_route(Agent.direct(ctx.client), ctx.target)
    assert is_map(route), "expected an authenticated direct route"
    original_deadline = route.deadline

    outage(ctx)
    assert {:ok, %{body: ^body}} = Protocol.request(ctx.client, ctx.uri, body)
    assert Direct.route(Agent.direct(ctx.client), ctx.target).deadline == original_deadline
    assert eventually(fn -> Direct.route(Agent.direct(ctx.client), ctx.target) == nil end, 5_000)
    assert {:error, _} = Protocol.request(ctx.client, ctx.uri, body, timeout_ms: 500)
    assert Agent.info(ctx.client).relay_discovery
  end

  test "the provider dials a reachable caller when only the caller allows listening" do
    ctx = pair(:caller)
    assert {:ok, %{body: "reverse"}} = Protocol.request(ctx.client, ctx.uri, "reverse")
    route = await_route(Agent.direct(ctx.client), ctx.target)
    assert is_map(route), "reverse dialing did not promote"
    manager_state = :sys.get_state(Agent.direct(ctx.client))
    assert manager_state.routes[route.generation].selected == "caller"
  end

  test "mutual hole punching reuses relay endpoints without configured listeners" do
    ctx =
      pair(:neither, client_rule: %{"hole_punch" => true}, provider_rule: %{"hole_punch" => true})

    [client_identity, provider_identity] = ctx.identities
    {:ok, client_endpoint} = Arc.Net.relay_endpoint(client_identity.public_key)
    {:ok, provider_endpoint} = Arc.Net.relay_endpoint(provider_identity.public_key)
    body = <<0, 255, 128, 10, 65>>

    assert {:ok, %{body: ^body}} = Protocol.request(ctx.client, ctx.uri, body)
    manager = Agent.direct(ctx.client)
    route = await_route(manager, ctx.target)
    assert is_map(route), "expected promotion from the relay-observed endpoints"
    internal = :sys.get_state(manager).routes[route.generation]
    assert internal.selected == "punch"
    assert internal.punch_context == client_endpoint
    assert internal.peer_punch == provider_endpoint.local
    assert internal.rule.listen == nil
    assert map_size(internal.connections) == 1

    outage(ctx)
    assert {:ok, %{body: ^body}} = Protocol.request(ctx.client, ctx.uri, body)
    assert Direct.route(manager, ctx.target).generation == route.generation
  end

  test "one-sided hole punching consent keeps the request on relays" do
    ctx = pair(:neither, client_rule: %{"hole_punch" => true})
    assert {:ok, %{body: "relay"}} = Protocol.request(ctx.client, ctx.uri, "relay")
    assert Direct.route(Agent.direct(ctx.client), ctx.target) == nil
    assert eventually(fn -> Direct.status(Agent.direct(ctx.provider)) == [] end)
  end

  test "a relay-observed address outside the owner's allowlist cannot promote" do
    ctx =
      pair(:neither,
        client_rule: %{"hole_punch" => true, "dial" => ["127.0.0.2"]},
        provider_rule: %{"hole_punch" => true}
      )

    assert {:ok, %{body: "relay"}} = Protocol.request(ctx.client, ctx.uri, "relay")
    assert Direct.route(Agent.direct(ctx.client), ctx.target) == nil
    assert eventually(fn -> Direct.status(Agent.direct(ctx.provider)) == [] end)
  end

  test "a maximum-sized request remains byte-exact over the promoted path" do
    ctx = pair(:provider)
    body = :binary.copy(<<0, 255, 128, 10>>, div(Protocol.max_body_bytes(), 4))
    assert {:ok, %{body: ^body}} = Protocol.request(ctx.client, ctx.uri, body)
    assert is_map(await_route(Agent.direct(ctx.client), ctx.target))
  end

  test "a stalled application cannot accumulate direct bodies beyond its queue bound" do
    ctx = pair(:provider, observer: self())
    assert {:ok, %{body: "warm"}} = Protocol.request(ctx.client, ctx.uri, "warm")
    manager = Agent.direct(ctx.client)
    provider_manager = Agent.direct(ctx.provider)
    route = await_route(manager, ctx.target)
    :sys.suspend(ctx.provider)

    try do
      for _ <- 1..16, do: Agent.poll_mailbox(ctx.provider)

      payload =
        Arc.Data.Frame.encode_request(
          Arc.Data.Frame.new_request_id(),
          %{"method" => "RAW", "path" => "/main", "capability_id" => "primary"},
          "overloaded"
        )

      assert :ok = Direct.send_packet(manager, route.generation, payload)
      assert eventually(fn -> not Direct.admitted?(provider_manager, route.generation) end)
    after
      :sys.resume(ctx.provider)
    end

    refute_receive {:arc_serve_event, %{body: "overloaded"}}, 100
  end

  test "a peer without approval continues on relays without opening a direct path" do
    ctx = pair(:denied)
    assert {:ok, %{body: "relay"}} = Protocol.request(ctx.client, ctx.uri, "relay")
    assert Direct.route(Agent.direct(ctx.client), ctx.target) == nil
    assert Agent.direct(ctx.provider) == nil
  end

  test "restoring the same pinned relay renews consent without replacing a healthy direct connection" do
    ctx = pair(:provider, lease_ms: 6_000)
    assert {:ok, %{body: "first"}} = Protocol.request(ctx.client, ctx.uri, "first")
    manager = Agent.direct(ctx.client)
    original = await_route(manager, ctx.target)
    assert is_map(original)
    outage(ctx)
    assert {:ok, %{body: "during"}} = Protocol.request(ctx.client, ctx.uri, "during")
    {:ok, restored} = Relay.start_link(ctx.port, relay_public_key: ctx.relay_key)
    on_exit(fn -> stop(restored) end)

    assert eventually(
             fn ->
               case Direct.route(manager, ctx.target) do
                 %{generation: generation, deadline: deadline} ->
                   generation == original.generation and deadline > original.deadline

                 _ ->
                   false
               end
             end,
             5_000
           )

    assert {:ok, %{body: "after"}} = Protocol.request(ctx.client, ctx.uri, "after")
  end

  test "a committed operation is not repeated and its late reply cannot move onto relays" do
    root =
      Path.join(System.tmp_dir!(), "arc-direct-receipt-#{System.unique_integer([:positive])}")

    File.mkdir_p!(root)
    receipts = Path.join(root, "receipts")
    previous_receipts = System.get_env("ARC_TEST_DIRECT_RECEIPTS")
    System.put_env("ARC_TEST_DIRECT_RECEIPTS", receipts)

    on_exit(fn ->
      if previous_receipts,
        do: System.put_env("ARC_TEST_DIRECT_RECEIPTS", previous_receipts),
        else: System.delete_env("ARC_TEST_DIRECT_RECEIPTS")

      File.rm_rf!(root)
    end)

    ctx = pair(:provider, lease_ms: 2_000, runtime: "direct-counter-provider.exs")
    assert {:ok, %{body: "warm"}} = Protocol.request(ctx.client, ctx.uri, "warm")
    route = await_route(Agent.direct(ctx.client), ctx.target)
    assert is_map(route)
    outage(ctx)
    Process.sleep(max(0, route.deadline - System.monotonic_time(:millisecond) - 200))

    pending =
      Task.async(fn -> Protocol.request(ctx.client, ctx.uri, "commit", timeout_ms: 1_200) end)

    assert eventually(fn -> File.exists?(receipts) end, 300)
    assert File.read!(receipts) == "committed\n"
    assert eventually(fn -> not Direct.admitted?(Agent.direct(ctx.client), route.generation) end)
    {:ok, restored} = Relay.start_link(ctx.port, relay_public_key: ctx.relay_key)
    on_exit(fn -> stop(restored) end)
    provider_sequence = :sys.get_state(ctx.provider).sessions[hd(ctx.identities).public_key].seq
    assert {:error, :outcome_unknown} = Task.await(pending, 2_000)
    assert File.read!(receipts) == "committed\n"

    assert :sys.get_state(ctx.provider).sessions[hd(ctx.identities).public_key].seq ==
             provider_sequence

    refute Direct.admitted?(Agent.direct(ctx.client), route.generation)
  end

  test "local withdrawal stops admission and an old generation cannot be renewed" do
    ctx = pair(:provider)
    assert {:ok, %{body: "first"}} = Protocol.request(ctx.client, ctx.uri, "first")
    manager = Agent.direct(ctx.client)
    route = await_route(manager, ctx.target)
    assert :ok = Direct.revoke(manager, route.generation)
    refute Direct.admitted?(manager, route.generation)
    assert {:error, :direct_unavailable} = Direct.send_packet(manager, route.generation, "old")
    assert {:ok, %{body: "relay"}} = Protocol.request(ctx.client, ctx.uri, "relay")
    assert Direct.route(manager, ctx.target) == nil
  end

  test "direct packets cannot enter through a relay after their generation is retired" do
    ctx = pair(:provider, observer: self())
    assert {:ok, %{body: "first"}} = Protocol.request(ctx.client, ctx.uri, "first")
    manager = Agent.direct(ctx.client)
    route = await_route(manager, ctx.target)
    session = :sys.get_state(manager).routes[route.generation].session
    request_id = Arc.Data.Frame.new_request_id()

    payload =
      Arc.Data.Frame.encode_request(
        request_id,
        %{"method" => "RAW", "path" => "/main", "capability_id" => "primary"},
        "stale"
      )

    {nonce, ciphertext, seq, _} = Arc.Data.Session.encrypt(session, payload)

    packet =
      Arc.Data.Packet.encode(
        hd(ctx.identities),
        ctx.target.key,
        session.session_id,
        seq,
        nonce,
        ciphertext,
        ek: session.ek_pub
      )

    assert :ok = Direct.revoke(manager, route.generation)
    provider_manager = Agent.direct(ctx.provider)
    assert eventually(fn -> not Direct.admitted?(provider_manager, route.generation) end)
    assert :ok = Arc.Net.deliver_via_relay(hd(ctx.identities).public_key, ctx.target.key, packet)
    refute_receive {:arc_serve_event, %{body: "stale"}}, 150
    assert [] == Agent.take_inbox(ctx.client, &(&1[:request_id] == request_id))

    assert Direct.reserved_session?(
             provider_manager,
             hd(ctx.identities).public_key,
             session.session_id
           )
  end

  test "crossed offers select the same initiator without abandoning both attempts" do
    ctx = pair(:both)
    [client_id, provider_id] = ctx.identities

    {:ok, reverse} =
      Protocol.parse("binary-echo+arc://#{Identity.encode_public_key(client_id)}/main")

    {:ok, client_entry} = Agent.connect(ctx.provider, Identity.encode_public_key(client_id))
    {:ok, provider_entry} = Agent.connect(ctx.client, Identity.encode_public_key(provider_id))

    {:ok, client_package} =
      Arc.Data.CapabilityManifest.detail(client_id, :sys.get_state(ctx.client).handler, "primary")

    {:ok, provider_package} =
      Arc.Data.CapabilityManifest.detail(
        provider_id,
        :sys.get_state(ctx.provider).handler,
        "primary"
      )

    client_manager = Agent.direct(ctx.client)
    provider_manager = Agent.direct(ctx.provider)
    :sys.suspend(ctx.client)
    :sys.suspend(ctx.provider)

    attempts =
      try do
        tasks = [
          Task.async(fn ->
            Direct.promote(
              client_manager,
              ctx.target,
              provider_package,
              provider_entry.x25519_public
            )
          end),
          Task.async(fn ->
            Direct.promote(provider_manager, reverse, client_package, client_entry.x25519_public)
          end)
        ]

        assert eventually(fn ->
                 length(Direct.status(client_manager)) == 1 and
                   length(Direct.status(provider_manager)) == 1
               end)

        tasks
      after
        :sys.resume(ctx.client)
        :sys.resume(ctx.provider)
      end

    results = Enum.map(attempts, &Task.await(&1, 5_000))
    winner_index = if client_id.public_key < provider_id.public_key, do: 0, else: 1
    assert {:ok, %{generation: generation}} = Enum.at(results, winner_index)
    assert {:error, :superseded} = Enum.at(results, 1 - winner_index)
    assert [%{generation: ^generation, phase: :active}] = Direct.status(client_manager)
    assert [%{generation: ^generation, phase: :active}] = Direct.status(provider_manager)
  end

  defp pair(direction, opts \\ []) do
    relay_key = Identity.generate().public_key
    {:ok, relay} = Relay.start_link(0, relay_public_key: relay_key)
    port = Relay.get_port(relay)
    client_identity = Identity.generate()
    provider_identity = Identity.generate()
    lease = Keyword.get(opts, :lease_ms, 10_000)

    client_rules = [
      rule(provider_identity.public_key, direction in [:caller, :both], lease)
      |> Map.merge(Keyword.get(opts, :client_rule, %{}))
    ]

    provider_rules =
      if direction == :denied,
        do: [],
        else: [
          rule(client_identity.public_key, direction in [:provider, :both], lease)
          |> Map.merge(Keyword.get(opts, :provider_rule, %{}))
        ]

    runtime = Path.join(@fixtures, Keyword.get(opts, :runtime, "binary-echo-provider.exs"))
    manifest = Path.join(@fixtures, "binary-echo-provider.json")
    serve = "exec://#{runtime}?#{URI.encode_query(%{"manifest" => manifest})}"
    client_opts = [direct_policy: client_rules]

    client_opts =
      if direction == :both, do: Keyword.put(client_opts, :serve, serve), else: client_opts

    {:ok, client} = Agent.start_link(client_identity, client_opts)

    {:ok, provider} =
      Agent.start_link(provider_identity,
        direct_policy: provider_rules,
        observer: Keyword.get(opts, :observer),
        serve: serve
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
      relay: relay,
      relay_key: relay_key,
      port: port,
      uri: uri,
      target: target,
      identities: [client_identity, provider_identity]
    }
  end

  defp rule(peer, listen?, lease) do
    rule = %{
      "peer" => Base.encode16(peer, case: :lower),
      "capability" => "primary",
      "scheme" => "binary-echo",
      "path" => "/main",
      "lease_ms" => lease,
      "dial" => ["127.0.0.1"]
    }

    if listen?,
      do:
        Map.put(rule, "listen", %{"bind" => "127.0.0.1", "address" => "127.0.0.1", "port" => 0}),
      else: rule
  end

  defp outage(ctx) do
    stop(ctx.relay)

    for identity <- ctx.identities do
      {:ok, transport} = TransportManager.lookup(identity.public_key)
      conn = :sys.get_state(transport).relay_conn
      if is_pid(conn) and Process.alive?(conn), do: Process.exit(conn, :shutdown)
    end

    assert eventually(fn ->
             Enum.all?(ctx.identities, fn identity ->
               Arc.Net.relay_public_key(identity.public_key) == {:error, :relay_not_connected}
             end)
           end)
  end

  defp stop(pid) do
    Arc.Net.TestTeardown.stop(pid)
  catch
    :exit, _ -> :ok
  end

  # Promotion is negotiated over the relay and can reach its active phase just
  # after the relayed request returns, so read the route through a bounded wait.
  defp await_route(manager, target) do
    deadline = System.monotonic_time(:millisecond) + 2_000
    wait_route(manager, target, deadline)
  end

  defp wait_route(manager, target, deadline) do
    route = Direct.route(manager, target)

    cond do
      is_map(route) ->
        route

      System.monotonic_time(:millisecond) >= deadline ->
        route

      true ->
        Process.sleep(20)
        wait_route(manager, target, deadline)
    end
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
