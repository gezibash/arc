defmodule Arc.Net.RelayObservationTest do
  use ExUnit.Case, async: false

  alias Arc.Data.RelayAnnouncement
  alias Arc.Identity
  alias Arc.Net.{Relay, Transport, TransportManager}

  setup do
    Application.ensure_all_started(:arc_net)
    :ok = TransportManager.reset()
    {:ok, relay} = Relay.start_link(0)
    port = Relay.get_port(relay)

    on_exit(fn ->
      TransportManager.reset()
      stop(relay)
    end)

    %{relay: relay, port: port}
  end

  test "returns only the requesting identity's current relay observation", %{
    port: port,
    relay: relay
  } do
    alice = Identity.generate()
    bob = Identity.generate()
    relay_key = Relay.get_pubkey(relay)

    assert :ok = Arc.Net.connect_relay(~c"127.0.0.1", port, alice, relay_key)
    assert :ok = Arc.Net.connect_relay(~c"127.0.0.1", port, bob, relay_key)

    assert {:ok, %{connection: alice_conn, local: {alice_ip, alice_port}, observed: observed}} =
             Arc.Net.relay_endpoint(alice.public_key)

    assert observed == %{"host" => List.to_string(:inet.ntoa(alice_ip)), "port" => alice_port}
    assert Arc.Net.relay_endpoint_current?(alice.public_key, alice_conn)

    assert {:ok, bob_transport} = TransportManager.lookup(bob.public_key)

    assert {:ok, %{"ok" => false, "error" => "invalid_request"}} =
             Transport.directory_request(bob_transport, :observe, %{
               "target" => Identity.encode_public_key(alice)
             })

    announcement =
      RelayAnnouncement.create(bob, [
        %{
          "id" => "files",
          "kind" => "storage",
          "title" => "Private files",
          "summary" => "Encrypted file storage"
        }
      ])

    assert :ok = Arc.Net.announce(bob.public_key, announcement)
    assert {:ok, %{entries: entries}} = Arc.Net.discover_via_relay(alice.public_key, "")
    assert Enum.any?(entries, &(&1.public_key == bob.public_key))

    assert Enum.all?(
             entries,
             &(not Map.has_key?(&1, :observed) and not Map.has_key?(&1, "observed"))
           )
  end

  test "rejects an unconnected identity", %{port: port} do
    identity = Identity.generate()
    assert {:error, :relay_not_connected} = Arc.Net.relay_endpoint(identity.public_key)
    assert false == Arc.Net.relay_endpoint_current?(identity.public_key, self())

    # Keep the local relay alive long enough to ensure this is a client-state failure.
    assert is_integer(port)
  end

  test "marks an observation stale after the relay connection is replaced", %{
    port: port,
    relay: relay
  } do
    identity = Identity.generate()
    relay_key = Relay.get_pubkey(relay)

    assert :ok = Arc.Net.connect_relay(~c"127.0.0.1", port, identity, relay_key)
    assert {:ok, %{connection: old_conn}} = Arc.Net.relay_endpoint(identity.public_key)
    assert {:ok, transport} = TransportManager.lookup(identity.public_key)

    Process.exit(old_conn, :shutdown)

    assert eventually(fn ->
             case Transport.relay_endpoint_context(transport) do
               {:ok, %{connection: conn}} -> conn != old_conn
               _ -> false
             end
           end)

    refute Arc.Net.relay_endpoint_current?(identity.public_key, old_conn)
  end

  defp eventually(fun, attempts \\ 80)

  defp eventually(fun, attempts) when attempts > 0 do
    if fun.(),
      do: true,
      else:
        (
          Process.sleep(25)
          eventually(fun, attempts - 1)
        )
  end

  defp eventually(_fun, 0), do: false

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
end
