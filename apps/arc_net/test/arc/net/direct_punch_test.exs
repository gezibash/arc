defmodule Arc.Net.DirectPunchTest do
  use ExUnit.Case, async: false

  alias Arc.Identity
  alias Arc.Net.Direct

  setup do
    Application.ensure_all_started(:ssl)
    alice = Identity.generate()
    bob = Identity.generate()
    {:ok, alice_credentials} = Direct.credentials()
    {:ok, bob_credentials} = Direct.credentials()

    %{
      alice: alice,
      bob: bob,
      alice_credentials: alice_credentials,
      bob_credentials: bob_credentials,
      binding: :crypto.strong_rand_bytes(32)
    }
  end

  test "punches from the source ports of live relay sockets and delivers packets", ctx do
    {alice_port, alice_relay} = relay_socket()
    {bob_port, bob_relay} = relay_socket()
    on_exit(fn -> close_all([alice_relay, bob_relay]) end)

    {alice_task, bob_task} =
      punch_pair(ctx, alice_port, bob_port)

    assert {:ok, alice} = Task.await(alice_task, 4_000)
    assert {:ok, bob} = Task.await(bob_task, 4_000)
    assert_receive {:arc_direct_connected, :alice, ^alice}, 2_000
    assert_receive {:arc_direct_connected, :bob, ^bob}, 2_000
    assert :ok = :gen_tcp.send(elem(alice_relay, 0), "relay remains open")
    assert_receive {:relay_packet, ^alice_port, "relay remains open"}, 2_000

    packet = :crypto.strong_rand_bytes(4_096)
    assert :ok = Direct.send_packet(alice, packet)
    assert_receive {:arc_direct_packet, :bob, ^bob, ^packet}, 2_000

    Direct.close(alice)
    Direct.close(bob)
  end

  test "rejects a wrong certificate pin and releases the local port", ctx do
    {alice_port, alice_relay} = relay_socket()
    {bob_port, bob_relay} = relay_socket()
    owner = self()

    alice_task =
      Task.async(fn ->
        punch(
          owner,
          {ctx.alice, ctx.alice_credentials},
          {ctx.bob, ctx.bob_credentials},
          ctx.binding,
          :alice,
          {alice_port, bob_port},
          :client,
          :crypto.strong_rand_bytes(32)
        )
      end)

    bob_task =
      Task.async(fn ->
        punch(
          owner,
          {ctx.bob, ctx.bob_credentials},
          {ctx.alice, ctx.alice_credentials},
          ctx.binding,
          :bob,
          {bob_port, alice_port},
          :server
        )
      end)

    assert {:error, _reason} = Task.await(alice_task, 4_000)
    assert {:error, _reason} = Task.await(bob_task, 4_000)
    refute_receive {:arc_direct_connected, _tag, _pid}, 100
    close_all([alice_relay, bob_relay])
    assert_eventually_listens(alice_port)
    assert_eventually_listens(bob_port)
  end

  test "caller death closes a pending carrier while its owner survives", ctx do
    local_port = unused_port()
    owner = self()

    caller =
      spawn(fn ->
        Direct.punch(owner,
          identity: ctx.alice,
          credentials: ctx.alice_credentials,
          peer_key: ctx.bob.public_key,
          peer_fingerprint: ctx.bob_credentials.fingerprint,
          binding: ctx.binding,
          tag: :alice,
          host: {127, 0, 0, 1},
          port: unused_port(),
          local_ip: {127, 0, 0, 1},
          local_port: local_port,
          role: :client,
          timeout_ms: 4_000
        )
      end)

    Process.sleep(100)
    Process.exit(caller, :shutdown)
    assert_eventually_listens(local_port)
    assert Process.alive?(owner)
  end

  defp punch_pair(ctx, alice_port, bob_port) do
    owner = self()

    alice =
      Task.async(fn ->
        punch(
          owner,
          {ctx.alice, ctx.alice_credentials},
          {ctx.bob, ctx.bob_credentials},
          ctx.binding,
          :alice,
          {alice_port, bob_port},
          :client
        )
      end)

    bob =
      Task.async(fn ->
        punch(
          owner,
          {ctx.bob, ctx.bob_credentials},
          {ctx.alice, ctx.alice_credentials},
          ctx.binding,
          :bob,
          {bob_port, alice_port},
          :server
        )
      end)

    {alice, bob}
  end

  defp punch(
         owner,
         {identity, credentials},
         {peer, peer_credentials},
         binding,
         tag,
         {local_port, peer_port},
         role,
         peer_fingerprint \\ nil
       ) do
    Direct.punch(owner,
      identity: identity,
      credentials: credentials,
      peer_key: peer.public_key,
      peer_fingerprint: peer_fingerprint || peer_credentials.fingerprint,
      binding: binding,
      tag: tag,
      host: {127, 0, 0, 1},
      port: peer_port,
      local_ip: {127, 0, 0, 1},
      local_port: local_port,
      role: role,
      timeout_ms: 3_000
    )
  end

  defp relay_socket do
    parent = self()
    listener = listen(0)
    port = port(listener)
    source_port = unused_port()

    {:ok, socket} =
      :gen_tcp.connect(
        {127, 0, 0, 1},
        port,
        [:binary, active: false, ip: {127, 0, 0, 1}, port: source_port, reuseaddr: true],
        1_000
      )

    acceptor =
      spawn(fn ->
        {:ok, accepted} = :gen_tcp.accept(listener, 1_000)

        case :gen_tcp.recv(accepted, 0, 3_000) do
          {:ok, packet} -> send(parent, {:relay_packet, source_port, packet})
          _ -> :ok
        end

        receive do
          :close -> :gen_tcp.close(accepted)
        end
      end)

    {source_port, {socket, listener, acceptor}}
  end

  defp close_all(sockets) do
    Enum.each(sockets, fn {socket, listener, acceptor} ->
      :gen_tcp.close(socket)
      :gen_tcp.close(listener)
      send(acceptor, :close)
    end)
  end

  defp assert_eventually_listens(port, attempts \\ 20)
  defp assert_eventually_listens(_port, 0), do: flunk("punch listener leaked")

  defp assert_eventually_listens(port, attempts) do
    case :gen_tcp.listen(port, [:binary, active: false, ip: {127, 0, 0, 1}, reuseaddr: true]) do
      {:ok, socket} ->
        :gen_tcp.close(socket)

      {:error, _reason} ->
        Process.sleep(25)
        assert_eventually_listens(port, attempts - 1)
    end
  end

  defp unused_port do
    socket = listen(0)
    value = port(socket)
    :gen_tcp.close(socket)
    value
  end

  defp listen(port),
    do: elem(:gen_tcp.listen(port, [:binary, active: false, ip: {127, 0, 0, 1}]), 1)

  defp port(socket), do: elem(:inet.port(socket), 1)
end
