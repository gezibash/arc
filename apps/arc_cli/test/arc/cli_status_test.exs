defmodule Arc.CLI.StatusTest do
  use ExUnit.Case, async: false

  import ExUnit.CaptureIO

  alias Arc.Host.Service
  alias Arc.Identity
  alias Arc.Identity.KeyStore
  alias Arc.Net.Relay
  alias Arc.Net.TransportManager

  setup do
    root = Path.join(System.tmp_dir!(), "arc-status-#{System.unique_integer([:positive])}")
    File.mkdir_p!(root)
    previous_cwd = File.cwd!()

    for key <- ["ARC_KEY", "ARC_RELAY", "ARC_RELAY_PUBKEY"] do
      previous = System.get_env(key)
      System.delete_env(key)

      on_exit(fn ->
        if previous, do: System.put_env(key, previous), else: System.delete_env(key)
      end)
    end

    for {key, path} <- [keys_dir: "keys", default_file: "default.key"] do
      previous = Application.fetch_env(:arc_identity, key)
      Application.put_env(:arc_identity, key, Path.join(root, path))

      on_exit(fn ->
        case previous do
          {:ok, value} -> Application.put_env(:arc_identity, key, value)
          :error -> Application.delete_env(:arc_identity, key)
        end
      end)
    end

    File.cd!(root)
    TransportManager.reset()

    on_exit(fn ->
      TransportManager.reset()
      File.cd!(previous_cwd)
      File.rm_rf!(root)
    end)

    %{root: root, socket: Path.join(root, "host.sock")}
  end

  test "fresh setup reports missing identity, local delivery and no host", ctx do
    {result, report} = report(ctx)
    assert result == nil
    assert report["identity"]["status"] == "not_selected"
    assert report["relay"]["status"] == "local"
    assert report["host"]["status"] == "not_running"
    refute File.exists?(Path.join(ctx.root, "keys"))
    refute File.exists?(Path.join(ctx.root, "default.key"))
  end

  test "identity follows the current directory and environment selectors", ctx do
    {:ok, local} = KeyStore.generate()
    {:ok, global} = KeyStore.generate()
    KeyStore.set_default(Identity.name(global))
    File.write!("arc.key", Identity.name(local))

    {_, selected} = report(ctx)
    assert selected["identity"]["public_key"] == Identity.encode_public_key(local)
    assert selected["identity"]["source"] == Path.join(File.cwd!(), "arc.key")

    System.put_env("ARC_KEY", Identity.name(global))
    {_, overridden} = report(ctx)
    assert overridden["identity"]["public_key"] == Identity.encode_public_key(global)
    assert overridden["identity"]["source"] == "ARC_KEY"
    refute :json.encode(overridden) |> IO.iodata_to_binary() =~ Base.encode16(global.secret_key)
  end

  test "bad identity selector fails while keeping the diagnostic overview", ctx do
    File.write!("arc.key", "")
    {result, report} = report(ctx)
    assert result == {:exit, 1}
    assert report["identity"]["status"] == "error"
    assert report["identity"]["message"] =~ "Invalid identity selector"
    assert report["host"]["status"] == "not_running"
  end

  test "configured relay is not presented as an established connection", ctx do
    System.put_env("ARC_RELAY", "127.0.0.1:7331")
    {_, report} = report(ctx)
    assert report["relay"]["status"] == "configured"
    assert report["relay"]["check"]["status"] == "not_checked"
    assert TransportManager.count() == 0

    output = capture_io(fn -> Arc.CLI.main(["status", "--no-docker", "--socket", ctx.socket]) end)
    assert output =~ "Client mode: on demand"
    assert output =~ "Local host: not running"
    refute output =~ "connected"
  end

  test "explicit relay options override shell settings", ctx do
    System.put_env("ARC_RELAY", "invalid")
    System.put_env("ARC_RELAY_PUBKEY", "invalid")
    pin = Identity.generate() |> Identity.encode_public_key()
    {_, report} = report(ctx, ["--relay", "localhost:1234", "--relay-pubkey", pin])
    assert report["relay"]["host"] == "localhost"
    assert report["relay"]["port"] == 1234
    assert report["relay"]["source"] == "--relay"
    assert report["relay"]["pinned"]
  end

  test "invalid relay configuration fails without echoing the supplied value", ctx do
    for opts <- [
          ["--relay", "invalid-sensitive-value"],
          ["--relay-pubkey", "invalid-sensitive-value"],
          ["--relay", "localhost:7331", "--relay-pubkey", "invalid-sensitive-value"]
        ] do
      {result, report} = report(ctx, opts)
      assert result == {:exit, 1}
      assert report["relay"]["status"] == "error"
      refute :json.encode(report) |> IO.iodata_to_binary() =~ "invalid-sensitive-value"
    end
  end

  test "greeting check leaves an existing citizen route intact", ctx do
    relay = start_supervised!({Relay, 0})
    port = Relay.get_port(relay)
    pin = Relay.get_pubkey(relay)
    {:ok, identity} = KeyStore.generate()
    KeyStore.set_default(Identity.name(identity))
    :ok = Arc.Net.connect_relay(~c"127.0.0.1", port, identity, pin)
    {:ok, %{connection: conn}} = Arc.Net.relay_endpoint(identity.public_key)
    original_route = Relay.route_for(relay, identity.public_key)
    assert is_pid(original_route)

    {result, report} =
      report(ctx, [
        "--check",
        "--relay",
        "127.0.0.1:#{port}",
        "--relay-pubkey",
        Base.encode16(pin)
      ])

    assert result == nil
    assert report["relay"]["check"]["status"] == "reachable"
    assert report["relay"]["check"]["pin_matches"]
    assert Arc.Net.relay_endpoint_current?(identity.public_key, conn)
    assert Relay.route_for(relay, identity.public_key) == original_route
    assert TransportManager.count() == 1
  end

  test "greeting check rejects a different relay key", ctx do
    relay = start_supervised!({Relay, 0})
    port = Relay.get_port(relay)
    pin = Identity.generate() |> Identity.encode_public_key()

    {result, report} =
      report(ctx, ["--check", "--relay", "127.0.0.1:#{port}", "--relay-pubkey", pin])

    assert result == {:exit, 1}
    assert report["relay"]["check"]["status"] == "key_mismatch"
    assert TransportManager.count() == 0
  end

  test "a listener that never speaks ARC produces a bounded failure", ctx do
    {:ok, listener} = :gen_tcp.listen(0, [:binary, active: false])
    {:ok, port} = :inet.port(listener)
    on_exit(fn -> :gen_tcp.close(listener) end)
    started = System.monotonic_time(:millisecond)
    {result, report} = report(ctx, ["--check", "--relay", "127.0.0.1:#{port}"])
    assert result == {:exit, 1}
    assert report["relay"]["check"]["status"] == "unreachable"
    assert System.monotonic_time(:millisecond) - started < 3_000
  end

  test "local host is shown as running without claiming a relay connection", ctx do
    start_supervised!({Service, socket_path: ctx.socket})
    {_, report} = report(ctx)
    assert report["host"]["status"] == "running"
    assert report["host"]["identity_count"] == 0
    refute Map.has_key?(report["host"], "admin_token_path")
    refute Map.has_key?(report["host"], "tokens")
  end

  test "selected identity connection comes from the host, separately from shell settings", ctx do
    relay = start_supervised!({Relay, 0})
    port = Relay.get_port(relay)
    pin = Relay.get_pubkey(relay)
    {:ok, identity} = KeyStore.generate()
    KeyStore.set_default(Identity.name(identity))

    host =
      start_supervised!(
        {Service, socket_path: ctx.socket, relay: {~c"127.0.0.1", port}, relay_pubkey: pin}
      )

    {:ok, token} = Service.read_admin_token(ctx.socket)
    {:ok, delegated} = Service.issue_token(host, token, identity: Identity.name(identity))

    {:ok, _} =
      Arc.Host.Client.request(ctx.socket, "initialize", %{"token" => delegated["token"]})

    System.put_env("ARC_RELAY", "shell-relay.example:7331")
    {_, report} = report(ctx)
    assert report["relay"]["host"] == "shell-relay.example"
    assert [%{"status" => "connected", "port" => ^port}] = report["host"]["relay_connections"]

    output = capture_io(fn -> Arc.CLI.main(["status", "--no-docker", "--socket", ctx.socket]) end)
    assert output =~ "Identity in host: connected (127.0.0.1:#{port})"
    refute output =~ token
    refute output =~ delegated["token"]
  end

  test "unresponsive host reports unavailable instead of stopped", ctx do
    {:ok, listener} = :socket.open(:local, :stream, :default)
    :ok = :socket.bind(listener, %{family: :local, path: String.to_charlist(ctx.socket)})
    :ok = :socket.listen(listener)
    on_exit(fn -> :socket.close(listener) end)
    started = System.monotonic_time(:millisecond)
    {result, report} = report(ctx)
    assert result == {:exit, 1}
    assert report["host"]["status"] == "unavailable"
    assert System.monotonic_time(:millisecond) - started < 2_000
  end

  test "unknown and incomplete options fail" do
    for args <- [["--relay"], ["extra"], ["--chek"]] do
      {result, stderr} = with_io(:stderr, fn -> Arc.CLI.main(["status" | args]) end)
      assert result == {:exit, 1}
      assert stderr =~ "usage: arc status"
    end
  end

  defp report(ctx, args \\ []) do
    {result, output} =
      with_io(fn ->
        Arc.CLI.main(["status", "--json", "--no-docker", "--socket", ctx.socket | args])
      end)

    {result, :json.decode(output)}
  end
end
