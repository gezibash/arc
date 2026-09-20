defmodule Arc.Net.DirectTest do
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

  test "pins both certificates, proves both ARC identities, and delivers framed bytes", ctx do
    {:ok, listener, port} = listen(ctx)
    {:ok, client} = connect(ctx, port)

    assert listener == assert_connected(:server)
    assert client == assert_connected(:client)

    packet = :crypto.strong_rand_bytes(32_768)
    assert :ok = Direct.send_packet(client, packet)
    assert_receive {:arc_direct_packet, :server, _server, ^packet}, 2_000

    Direct.close(client)
    Direct.close(listener)
  end

  test "certificate verification pins OTP records and preserves unknown extension checks", ctx do
    credentials = ctx.alice_credentials
    cert = :public_key.pkix_decode_cert(credentials.cert_der, :otp)

    assert {:valid, _} =
             Direct.verify_peer_certificate(cert, :valid_peer, credentials.fingerprint)

    assert {:fail, :certificate_pin_mismatch} =
             Direct.verify_peer_certificate(cert, :valid_peer, <<0::256>>)

    assert {:unknown, _} =
             Direct.verify_peer_certificate(cert, {:extension, :unknown}, credentials.fingerprint)
  end

  test "rejects a mismatched certificate pin before application admission", ctx do
    {:ok, listener, port} = listen(ctx)
    wrong_pin = :crypto.strong_rand_bytes(32)

    assert {:error, _reason} =
             Direct.connect(self(),
               identity: ctx.bob,
               credentials: ctx.bob_credentials,
               peer_key: ctx.alice.public_key,
               peer_fingerprint: wrong_pin,
               binding: ctx.binding,
               tag: :client,
               host: {127, 0, 0, 1},
               port: port,
               timeout_ms: 1_000
             )

    refute_receive {:arc_direct_connected, _tag, _pid}, 100
    Direct.close(listener)
  end

  test "rejects an agreed binding mismatch before application admission", ctx do
    {:ok, listener, port} = listen(ctx)

    assert {:error, _reason} =
             Direct.connect(self(),
               identity: ctx.bob,
               credentials: ctx.bob_credentials,
               peer_key: ctx.alice.public_key,
               peer_fingerprint: ctx.alice_credentials.fingerprint,
               binding: :crypto.strong_rand_bytes(32),
               tag: :client,
               host: {127, 0, 0, 1},
               port: port,
               timeout_ms: 1_000
             )

    refute_receive {:arc_direct_connected, _tag, _pid}, 100
    Direct.close(listener)
  end

  test "enforces the direct frame cap before it reaches TLS", ctx do
    {:ok, listener, port} = listen(ctx)
    {:ok, client} = connect(ctx, port)
    assert listener == assert_connected(:server)
    assert client == assert_connected(:client)

    assert {:error, :packet_too_large} =
             Direct.send_packet(client, :binary.copy(<<0>>, Direct.max_packet_bytes() + 1))

    Direct.close(client)
    Direct.close(listener)
  end

  test "refuses a queued direct send after its monotonic lease deadline", ctx do
    {:ok, listener, port} = listen(ctx)
    {:ok, client} = connect(ctx, port)
    assert listener == assert_connected(:server)
    assert client == assert_connected(:client)

    assert {:error, :lease_expired} =
             Direct.send_packet(client, "too late", System.monotonic_time(:millisecond) - 1)

    Direct.close(client)
    Direct.close(listener)
  end

  test "explicit close retires both sides and releases their replay claims", ctx do
    {:ok, listener, port} = listen(ctx)
    {:ok, client} = connect(ctx, port)
    assert listener == assert_connected(:server)
    assert client == assert_connected(:client)

    client_ref = Process.monitor(client)
    listener_ref = Process.monitor(listener)
    assert :ok = Direct.close(client)

    assert_receive {:arc_direct_closed, :client, ^client, :closed_by_owner}, 2_000
    assert_receive {:arc_direct_closed, :server, ^listener, :closed}, 2_000
    assert_receive {:DOWN, ^client_ref, :process, ^client, :normal}, 2_000
    assert_receive {:DOWN, ^listener_ref, :process, ^listener, :normal}, 2_000
    assert_registry_released(ctx.alice.public_key, ctx.binding)
    assert_registry_released(ctx.bob.public_key, ctx.binding)
  end

  test "closes when the manager-facing listener queue is full", ctx do
    {:ok, listener, port} = listen(ctx)
    {:ok, client} = connect(ctx, port)
    assert listener == assert_connected(:server)
    assert client == assert_connected(:client)

    listener_ref = Process.monitor(listener)
    for n <- 1..16, do: send(self(), {:test_backlog, n})
    assert :ok = Direct.send_packet(client, "overload listener owner")

    assert_receive {:arc_direct_closed, :server, ^listener, :owner_overloaded}, 2_000
    assert_receive {:DOWN, ^listener_ref, :process, ^listener, :normal}, 2_000
  end

  test "closes when the connection-to-listener queue is full", ctx do
    {:ok, listener, port} = listen(ctx)
    {:ok, client} = connect(ctx, port)
    assert listener == assert_connected(:server)
    assert client == assert_connected(:client)

    connection = :sys.get_state(listener).connection
    connection_ref = Process.monitor(connection)
    :ok = :sys.suspend(listener)
    for n <- 1..16, do: send(listener, {:test_backlog, n})
    assert {:message_queue_len, queued} = Process.info(listener, :message_queue_len)
    assert queued >= 16
    assert :ok = Direct.send_packet(client, "overload listener")
    assert_receive {:DOWN, ^connection_ref, :process, ^connection, :normal}, 2_000
    :ok = :sys.resume(listener)

    assert_receive {:arc_direct_closed, :server, ^listener, :owner_overloaded}, 2_000
  end

  defp listen(ctx) do
    Direct.listen(self(),
      identity: ctx.alice,
      credentials: ctx.alice_credentials,
      peer_key: ctx.bob.public_key,
      peer_fingerprint: ctx.bob_credentials.fingerprint,
      binding: ctx.binding,
      tag: :server,
      ip: {127, 0, 0, 1},
      port: 0,
      timeout_ms: 2_000
    )
  end

  defp connect(ctx, port) do
    Direct.connect(self(),
      identity: ctx.bob,
      credentials: ctx.bob_credentials,
      peer_key: ctx.alice.public_key,
      peer_fingerprint: ctx.alice_credentials.fingerprint,
      binding: ctx.binding,
      tag: :client,
      host: {127, 0, 0, 1},
      port: port,
      timeout_ms: 2_000
    )
  end

  defp assert_connected(tag) do
    assert_receive {:arc_direct_connected, ^tag, pid}, 2_000
    pid
  end

  defp assert_registry_released(identity_key, binding, attempts \\ 20)

  defp assert_registry_released(identity_key, binding, 0) do
    assert [] == Registry.lookup(Arc.Net.DirectRegistry, {identity_key, binding})
  end

  defp assert_registry_released(identity_key, binding, attempts) do
    case Registry.lookup(Arc.Net.DirectRegistry, {identity_key, binding}) do
      [] ->
        :ok

      _ ->
        Process.sleep(25)
        assert_registry_released(identity_key, binding, attempts - 1)
    end
  end
end
