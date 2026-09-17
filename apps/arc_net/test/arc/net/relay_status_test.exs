defmodule Arc.Net.RelayStatusTest do
  use ExUnit.Case, async: false

  alias Arc.Identity
  alias Arc.Net.{Handshake, Relay, RelayStatus, Transport, TransportManager}

  setup do
    Application.ensure_all_started(:arc_net)
    :ok = TransportManager.reset()
    {:ok, relay} = Relay.start_link(0)
    port = Relay.get_port(relay)

    on_exit(fn ->
      TransportManager.reset()
      stop(relay)
    end)

    %{relay: relay, port: port, relay_key: Relay.get_pubkey(relay)}
  end

  test "fetches a bounded public status document from a pinned relay", %{
    port: port,
    relay_key: relay_key
  } do
    assert {:ok, status} = RelayStatus.fetch(~c"127.0.0.1", port, relay_key)

    assert status["role"] == "relay"
    assert status["state"] == "running"
    assert is_binary(status["version"])
    assert status["public_key"] == Base.encode16(relay_key, case: :lower)
    assert is_integer(status["uptime_seconds"])
    assert status["uptime_seconds"] >= 0
    assert is_boolean(status["federation_transit"])

    assert MapSet.new(Map.keys(status)) ==
             MapSet.new([
               "role",
               "state",
               "version",
               "public_key",
               "uptime_seconds",
               "federation_transit"
             ])
  end

  test "rejects a status request containing extra fields", %{port: port, relay_key: relay_key} do
    {:ok, transport} = Transport.start_link()

    on_exit(fn -> stop(transport) end)

    assert :ok =
             Transport.connect_relay(
               transport,
               ~c"127.0.0.1",
               port,
               Identity.generate(),
               relay_key
             )

    assert {:ok, %{"ok" => false, "error" => "invalid_request"}} =
             Transport.directory_request(transport, :status, %{"extra" => true})
  end

  test "rejects a mismatched relay pin", %{port: port} do
    assert {:error, :relay_pubkey_mismatch} =
             RelayStatus.fetch(~c"127.0.0.1", port, :crypto.strong_rand_bytes(32))
  end

  test "cleans up its temporary relay route", %{port: port, relay: relay, relay_key: relay_key} do
    assert {:ok, %{"state" => "running"}} = RelayStatus.fetch(~c"127.0.0.1", port, relay_key)
    assert eventually(fn -> Relay.stats(relay).routes == 0 end)
  end

  test "bounds a stalled relay handshake" do
    original_timeout = Application.get_env(:arc_net, :relay_hello_timeout_ms)
    Application.put_env(:arc_net, :relay_hello_timeout_ms, 50)

    on_exit(fn ->
      if original_timeout == nil do
        Application.delete_env(:arc_net, :relay_hello_timeout_ms)
      else
        Application.put_env(:arc_net, :relay_hello_timeout_ms, original_timeout)
      end
    end)

    {:ok, listener} = :gen_tcp.listen(0, [:binary, packet: :raw, active: false, reuseaddr: true])
    {:ok, {_address, port}} = :inet.sockname(listener)

    task =
      Task.async(fn ->
        {:ok, socket} = :gen_tcp.accept(listener)
        Process.sleep(100)
        :gen_tcp.close(socket)
        :gen_tcp.close(listener)
      end)

    assert {:error, :timeout} = RelayStatus.fetch(~c"127.0.0.1", port)
    Task.await(task, 1_000)
  end

  test "treats an older relay's invalid status request as unsupported" do
    %{port: port, relay_key: relay_key, task: task} =
      start_fake_relay(fn socket, _relay_key ->
        request = recv_directory_request(socket)

        send_directory_reply(socket, %{
          "request_id" => request["request_id"],
          "ok" => false,
          "error" => "invalid_request"
        })
      end)

    assert {:error, :unsupported} = RelayStatus.fetch(~c"127.0.0.1", port, relay_key)
    Task.await(task, 1_000)
  end

  test "rejects a status response whose relay key differs from the handshake" do
    %{port: port, relay_key: relay_key, task: task} =
      start_fake_relay(fn socket, _relay_key ->
        request = recv_directory_request(socket)

        send_directory_reply(socket, %{
          "request_id" => request["request_id"],
          "ok" => true,
          "status" => fake_status(:crypto.strong_rand_bytes(32))
        })
      end)

    assert {:error, :status_unavailable} = RelayStatus.fetch(~c"127.0.0.1", port, relay_key)
    Task.await(task, 1_000)
  end

  test "rejects a status response with an unsafe version" do
    %{port: port, relay_key: relay_key, task: task} =
      start_fake_relay(fn socket, relay_key ->
        request = recv_directory_request(socket)

        send_directory_reply(socket, %{
          "request_id" => request["request_id"],
          "ok" => true,
          "status" => Map.put(fake_status(relay_key), "version", "0.3.2\nspoofed")
        })
      end)

    assert {:error, :status_unavailable} = RelayStatus.fetch(~c"127.0.0.1", port, relay_key)
    Task.await(task, 1_000)
  end

  test "terminates its temporary transport at the outer deadline" do
    owner = self()

    %{port: port, relay_key: relay_key, task: task} =
      start_fake_relay(fn socket, _relay_key ->
        Process.sleep(1_500)
        request = recv_directory_request(socket, 2_000)
        send(owner, {:status_requested, request["request_id"]})
        Process.sleep(1_700)
        send(owner, {:temporary_transport_closed, :gen_tcp.recv(socket, 1, 2_500)})
      end)

    {elapsed_us, result} = :timer.tc(fn -> RelayStatus.fetch(~c"127.0.0.1", port, relay_key) end)

    assert result == {:error, :timeout}
    assert elapsed_us >= 2_500_000
    assert elapsed_us < 4_000_000
    assert_receive {:status_requested, _request_id}, 500
    assert_receive {:temporary_transport_closed, {:error, :closed}}, 1_000
    Task.await(task, 1_000)
  end

  test "does not replace an existing citizen relay route", %{
    port: port,
    relay: relay,
    relay_key: relay_key
  } do
    citizen = Identity.generate()
    assert :ok = Arc.Net.connect_relay(~c"127.0.0.1", port, citizen, relay_key)
    assert {:ok, %{status: :connected}} = Arc.Net.relay_status(citizen.public_key)
    assert {:ok, transport} = TransportManager.lookup(citizen.public_key)
    assert {:ok, %{connection: citizen_connection}} = Transport.relay_endpoint_context(transport)
    assert eventually(fn -> is_pid(Relay.route_for(relay, citizen.public_key)) end)
    relay_connection = Relay.route_for(relay, citizen.public_key)
    assert is_pid(relay_connection)

    assert {:ok, %{"state" => "running"}} = RelayStatus.fetch(~c"127.0.0.1", port, relay_key)

    assert {:ok, %{status: :connected, host: "127.0.0.1", port: ^port}} =
             Arc.Net.relay_status(citizen.public_key)

    assert {:ok, %{connection: ^citizen_connection}} = Transport.relay_endpoint_context(transport)
    assert ^relay_connection = Relay.route_for(relay, citizen.public_key)
  end

  defp stop(pid) when is_pid(pid) do
    if Process.alive?(pid) do
      Process.unlink(pid)

      try do
        GenServer.stop(pid, :normal)
      catch
        :exit, _ -> :ok
      end
    end
  end

  defp eventually(fun, attempts \\ 40)

  defp eventually(fun, attempts) when attempts > 0 do
    if fun.() do
      true
    else
      Process.sleep(25)
      eventually(fun, attempts - 1)
    end
  end

  defp eventually(_fun, 0), do: false

  defp start_fake_relay(handler) do
    {:ok, listener} = :gen_tcp.listen(0, [:binary, packet: :raw, active: false, reuseaddr: true])
    {:ok, {_address, port}} = :inet.sockname(listener)
    relay_key = :crypto.strong_rand_bytes(32)
    challenge = :crypto.strong_rand_bytes(32)

    task =
      Task.async(fn ->
        {:ok, socket} = :gen_tcp.accept(listener)
        {:ok, hello} = Handshake.relay_hello(relay_key, challenge)
        :ok = :gen_tcp.send(socket, hello)
        :ok = :gen_tcp.send(socket, frame(Handshake.relay_info(:unbounded)))
        {:ok, _client_hello} = :gen_tcp.recv(socket, 96, 2_000)
        handler.(socket, relay_key)
        :gen_tcp.close(socket)
        :gen_tcp.close(listener)
      end)

    %{port: port, relay_key: relay_key, task: task}
  end

  defp recv_directory_request(socket, timeout \\ 2_000) do
    {:ok, <<length::32-big>>} = :gen_tcp.recv(socket, 4, timeout)
    {:ok, <<"ARC_DIRECTORY_V1", payload::binary>>} = :gen_tcp.recv(socket, length, timeout)
    :json.decode(payload)
  end

  defp send_directory_reply(socket, reply) do
    payload = reply |> Map.put("type", "reply") |> :json.encode() |> IO.iodata_to_binary()
    :ok = :gen_tcp.send(socket, frame("ARC_DIRECTORY_V1" <> payload))
  end

  defp fake_status(relay_key) do
    %{
      "role" => "relay",
      "state" => "running",
      "version" => "0.3.2",
      "public_key" => Base.encode16(relay_key, case: :lower),
      "uptime_seconds" => 1,
      "federation_transit" => false
    }
  end

  defp frame(payload), do: <<byte_size(payload)::32-big, payload::binary>>
end
