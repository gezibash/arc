defmodule Arc.Net.RelayHandlerTest do
  @moduledoc """
  Integration tests: exec-served ARC providers over an ARC relay.
  """

  use ExUnit.Case, async: false

  alias Arc.Data.Agent
  alias Arc.Data.Frame
  alias Arc.Data.Packet
  alias Arc.Data.Session
  alias Arc.Identity
  alias Arc.Net.Handshake

  setup do
    Application.ensure_all_started(:arc_net)
    Arc.Control.Local.reset()
    Arc.Net.TransportManager.reset()

    {:ok, relay} = Arc.Net.Relay.start_link(0)
    port = Arc.Net.Relay.get_port(relay)

    on_exit(fn ->
      Arc.Net.TransportManager.reset()
      stop_relay(relay)
    end)

    %{relay_port: port}
  end

  defp stop_relay(relay) do
    if Process.alive?(relay) do
      Process.unlink(relay)

      try do
        GenServer.stop(relay, :normal)
      catch
        :exit, _ -> :ok
      end
    end
  end

  defp publish_identity(id) do
    :ok = Arc.Control.publish(id)
    {x_pub, _} = Identity.to_x25519(id)
    :ok = Arc.Control.publish_keyex(id.public_key, x_pub)
  end

  defp relay_connect(port, identity) do
    {:ok, sock} = :gen_tcp.connect(~c"localhost", port, [:binary, packet: :raw, active: false])
    {:ok, relay_hello} = :gen_tcp.recv(sock, 64, 2_000)
    {:ok, relay_pubkey, relay_challenge} = Handshake.decode_relay_hello(relay_hello)

    {:ok, client_hello, _client_pubkey} =
      Handshake.client_hello(identity, relay_pubkey, relay_challenge)

    :ok = :gen_tcp.send(sock, client_hello)
    sock
  end

  defp client_session(client_id, server_id) do
    {server_x_pub, _} = Identity.to_x25519(server_id)
    Session.establish(client_id, server_id.public_key, server_x_pub)
  end

  defp send_request(sock, session, client_id, server_id, body, meta \\ %{}) do
    request_id = Frame.new_request_id()
    meta = if is_map(meta), do: meta, else: %{}
    meta = Map.put_new(meta, "method", "RAW")
    payload = Frame.encode_request(request_id, meta, body)
    send_frame_payload(sock, session, client_id, server_id, request_id, payload)
  end

  defp send_frame_payload(sock, session, client_id, server_id, request_id, payload) do
    {nonce, ciphertext, seq, session} = Session.encrypt(session, payload)

    packet =
      Packet.encode(
        client_id,
        server_id.public_key,
        session.session_id,
        seq,
        nonce,
        ciphertext
      )

    :ok = :gen_tcp.send(sock, <<byte_size(packet)::32-big, packet::binary>>)
    {session, request_id}
  end

  defp recv_reply_frame(sock, session, timeout \\ 3_000) do
    {:ok, <<len::32-big>>} = :gen_tcp.recv(sock, 4, timeout)
    {:ok, packet_bin} = :gen_tcp.recv(sock, len, timeout)
    {:ok, decoded} = Packet.decode(packet_bin)
    {:ok, plaintext} = Session.decrypt(session, decoded.nonce, decoded.ciphertext)
    Frame.decode(plaintext)
  end

  defp recv_reply(sock, session, timeout \\ 3_000) do
    {:ok, frame} = recv_reply_frame(sock, session, timeout)
    {:ok, frame.type, frame.body}
  end

  defp fetch_document(sock, session, client_id, server_id, path) do
    {session, request_id} =
      send_request(sock, session, client_id, server_id, "", %{"method" => "GET", "path" => path})

    {:ok, frame} = recv_reply_frame(sock, session)
    {session, request_id, frame, :json.decode(frame.body)}
  end

  defp start_exec_server(identity, runtime_path, manifest_path) do
    File.chmod!(runtime_path, 0o755)

    {:ok, agent} =
      Agent.start_link(
        identity,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(agent)
    {:ok, agent}
  end

  defp hello_provider_paths do
    runtime = Path.expand("../../../../../test/fixtures/providers/hello-provider.exs", __DIR__)
    manifest = Path.expand("../../../../../test/fixtures/providers/hello-provider.json", __DIR__)
    {runtime, manifest}
  end

  defp users_provider_paths do
    runtime = Path.expand("../../../../../test/fixtures/providers/users-provider.exs", __DIR__)
    manifest = Path.expand("../../../../../test/fixtures/providers/users-provider.json", __DIR__)
    {runtime, manifest}
  end

  defp sqlite_provider_paths do
    runtime = Path.expand("../../../../../test/fixtures/providers/sqlite-provider.exs", __DIR__)
    manifest = Path.expand("../../../../../test/fixtures/providers/sqlite-provider.json", __DIR__)
    {runtime, manifest}
  end

  defp sandbox_provider_paths do
    runtime = Path.expand("../../../../../test/fixtures/providers/sandbox-provider.exs", __DIR__)

    manifest =
      Path.expand("../../../../../test/fixtures/providers/sandbox-provider.json", __DIR__)

    {runtime, manifest}
  end

  test "hello provider round-trips through relay", %{relay_port: relay_port} do
    client_id = Identity.generate()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = hello_provider_paths()

    publish_identity(client_id)

    {:ok, server_agent} = start_exec_server(server_id, runtime_path, manifest_path)
    on_exit(fn -> if Process.alive?(server_agent), do: GenServer.stop(server_agent, :normal) end)

    :ok = Arc.Net.connect_relay(~c"localhost", relay_port, server_id)

    sock = relay_connect(relay_port, client_id)
    Process.sleep(100)

    session = client_session(client_id, server_id)
    {session, _request_id} = send_request(sock, session, client_id, server_id, "GET /")

    {:ok, type, body} = recv_reply(sock, session)
    assert type == :response
    assert body =~ "provider hello"
    assert body =~ "GET /"

    :gen_tcp.close(sock)
  end

  test "sqlite provider serves summary, detail, and invocation over relay", %{
    relay_port: relay_port
  } do
    client_id = Identity.generate()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = sqlite_provider_paths()

    publish_identity(client_id)

    {:ok, server_agent} = start_exec_server(server_id, runtime_path, manifest_path)
    on_exit(fn -> if Process.alive?(server_agent), do: GenServer.stop(server_agent, :normal) end)

    :ok = Arc.Net.connect_relay(~c"localhost", relay_port, server_id)

    sock = relay_connect(relay_port, client_id)
    Process.sleep(100)

    session = client_session(client_id, server_id)

    {session, summary_req_id, summary_frame, summary} =
      fetch_document(sock, session, client_id, server_id, "/info")

    assert summary_frame.type == :response
    assert summary_frame.request_id == summary_req_id
    assert summary["manifest_version"] == 2

    [capability] = summary["capabilities"]
    assert capability["kind"] == "data"
    assert capability["scheme"] == "sql"

    {session, detail_req_id, detail_frame, detail} =
      fetch_document(sock, session, client_id, server_id, capability["detail_path"])

    assert detail_frame.type == :response
    assert detail_frame.request_id == detail_req_id
    assert detail["capability"]["config"]["database"] == "demo.sqlite3"
    assert detail["capability"]["interfaces"]["cli"]["namespace"] == "sqlite"

    {session, _request_id} = send_request(sock, session, client_id, server_id, "SELECT 1 AS n")

    {:ok, type, body} = recv_reply(sock, session)
    assert type == :response
    assert body =~ "n"
    assert body =~ "1 row(s)"

    :gen_tcp.close(sock)
  end

  test "two exec-served identities can serve concurrently through the same relay", %{
    relay_port: relay_port
  } do
    client_one = Identity.generate()
    client_two = Identity.generate()
    server_one = Identity.generate()
    server_two = Identity.generate()
    {hello_runtime, hello_manifest} = hello_provider_paths()
    {users_runtime, users_manifest} = users_provider_paths()

    publish_identity(client_one)
    publish_identity(client_two)

    {:ok, server_agent_one} = start_exec_server(server_one, hello_runtime, hello_manifest)
    {:ok, server_agent_two} = start_exec_server(server_two, users_runtime, users_manifest)

    on_exit(fn ->
      if Process.alive?(server_agent_one), do: GenServer.stop(server_agent_one, :normal)
      if Process.alive?(server_agent_two), do: GenServer.stop(server_agent_two, :normal)
    end)

    :ok = Arc.Net.connect_relay(~c"localhost", relay_port, server_one)
    :ok = Arc.Net.connect_relay(~c"localhost", relay_port, server_two)

    sock_one = relay_connect(relay_port, client_one)
    sock_two = relay_connect(relay_port, client_two)
    Process.sleep(120)

    session_one = client_session(client_one, server_one)
    session_two = client_session(client_two, server_two)

    {session_one, _request_id} =
      send_request(sock_one, session_one, client_one, server_one, "GET /one")

    {session_two, _request_id} =
      send_request(sock_two, session_two, client_two, server_two, "GET /users")

    {:ok, :response, body_one} = recv_reply(sock_one, session_one)
    {:ok, :response, body_two} = recv_reply(sock_two, session_two)

    assert body_one =~ "provider hello"
    assert body_two =~ "ada@example.com"

    :gen_tcp.close(sock_one)
    :gen_tcp.close(sock_two)
  end

  test "stream frames round-trip through relay", %{relay_port: relay_port} do
    client_id = Identity.generate()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = sandbox_provider_paths()

    publish_identity(client_id)

    {:ok, server_agent} = start_exec_server(server_id, runtime_path, manifest_path)
    on_exit(fn -> if Process.alive?(server_agent), do: GenServer.stop(server_agent, :normal) end)

    :ok = Arc.Net.connect_relay(~c"localhost", relay_port, server_id)

    sock = relay_connect(relay_port, client_id)
    Process.sleep(100)

    session = client_session(client_id, server_id)
    request_id = Frame.new_request_id()
    app_session_id = "relay-shell-1"

    open_payload =
      Frame.encode_stream_open(
        request_id,
        %{"method" => "RAW", "path" => "/sandbox/shell", "app_session_id" => app_session_id},
        "SHELL sb-relay"
      )

    {session, ^request_id} =
      send_frame_payload(sock, session, client_id, server_id, request_id, open_payload)

    {:ok, open_frame} = recv_reply_frame(sock, session)
    assert open_frame.type == :stream_data
    assert open_frame.request_id == request_id
    assert open_frame.meta["app_session_id"] == app_session_id
    assert open_frame.body =~ "opened shell for sb-relay"

    data_payload =
      Frame.encode_stream_data(
        request_id,
        %{"method" => "RAW", "path" => "/sandbox/shell", "app_session_id" => app_session_id},
        "exit\n"
      )

    {session, ^request_id} =
      send_frame_payload(sock, session, client_id, server_id, request_id, data_payload)

    {:ok, exit_frame} = recv_reply_frame(sock, session)
    assert exit_frame.type == :stream_exit
    assert exit_frame.request_id == request_id
    assert exit_frame.meta["app_session_id"] == app_session_id
    assert exit_frame.meta["status"] == 0

    :gen_tcp.close(sock)
  end
end
