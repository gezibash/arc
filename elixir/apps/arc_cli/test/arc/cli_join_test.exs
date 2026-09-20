defmodule Arc.CLI.JoinTest do
  use ExUnit.Case, async: false
  import ExUnit.CaptureIO

  alias Arc.Identity
  alias Arc.Identity.KeyStore
  alias Arc.Net.{Relay, RelayConfig}

  setup do
    root = Path.join(System.tmp_dir!(), "arc-join-#{System.unique_integer([:positive])}")
    File.mkdir_p!(Path.join(root, "project"))
    previous_cwd = File.cwd!()
    File.cd!(Path.join(root, "project"))

    for {app, key, file} <- [
          {:arc_identity, :keys_dir, "keys"},
          {:arc_identity, :default_file, "default.key"},
          {:arc_net, :relay_config_path, "relays.json"}
        ] do
      previous = Application.fetch_env(app, key)
      Application.put_env(app, key, Path.join(root, file))

      on_exit(fn ->
        case previous do
          {:ok, value} -> Application.put_env(app, key, value)
          :error -> Application.delete_env(app, key)
        end
      end)
    end

    for key <- ["ARC_KEY", "ARC_RELAY", "ARC_RELAY_PUBKEY"] do
      old = System.get_env(key)
      System.delete_env(key)

      on_exit(fn ->
        if old, do: System.put_env(key, old), else: System.delete_env(key)
      end)
    end

    on_exit(fn ->
      File.cd!(previous_cwd)
      File.rm_rf!(root)
    end)

    relay = start_supervised!({Relay, 0})
    address = "localhost:#{Relay.get_port(relay)}"
    pin = Base.encode16(Relay.get_pubkey(relay), case: :lower)
    %{root: root, address: address, pin: pin}
  end

  test "first join confirms trust, creates identity, and enables status without flags", ctx do
    {result, output} = run([ctx.address], "yes\n")
    assert result == :ok
    assert output =~ ctx.pin
    assert output =~ "Joined #{ctx.address}"
    assert {:ok, identity} = KeyStore.resolve_active()
    assert output =~ Identity.name(identity)
    assert {:ok, %{address: address, public_key: pin}} = RelayConfig.resolve()
    assert address == ctx.address
    assert pin == ctx.pin

    output = capture_io(fn -> Arc.CLI.main(["status", "--format", "json"]) end)
    assert :json.decode(output)["state"] == "running"
    assert :json.decode(output)["address"] == ctx.address
  end

  test "a saved relay default carries discovery through the relay without flags", ctx do
    {:ok, identity} = KeyStore.generate()
    :ok = KeyStore.set_default(Identity.name(identity))
    :ok = RelayConfig.remember(ctx.address, ctx.pin)

    output = capture_io(fn -> Arc.CLI.main(["discover", "nothing-here"]) end)

    assert output =~ "Query: nothing-here"
    assert output =~ "Matches on this relay page: 0"
  end

  test "a malformed saved relay file prevents discovery from falling back locally" do
    {:ok, identity} = KeyStore.generate()
    :ok = KeyStore.set_default(Identity.name(identity))
    File.mkdir_p!(Path.dirname(RelayConfig.path()))
    File.write!(RelayConfig.path(), "not json")
    File.chmod!(RelayConfig.path(), 0o600)

    output =
      capture_io(:stderr, fn ->
        assert Arc.CLI.main(["discover", "nothing-here"]) == {:exit, 1}
      end)

    assert output =~ "relay settings are invalid"
  end

  test "declining or missing confirmation saves neither trust nor identity", ctx do
    for input <- ["no\n", ""] do
      capture_io(:stderr, fn ->
        {result, _} = run([ctx.address], input)
        assert result == {:exit, 1}
      end)

      refute File.exists?(RelayConfig.path())
      assert KeyStore.list() == []
    end
  end

  test "explicit pin supports unattended join and repeat join needs no prompt", ctx do
    {result, output} = run([ctx.address, "--relay-pubkey", ctx.pin])
    assert result == :ok
    refute output =~ "Trust and remember"
    assert {:ok, identity} = KeyStore.resolve_active()
    {result, output} = run([ctx.address])
    assert result == :ok
    refute output =~ "Trust and remember"
    assert {:ok, ^identity} = KeyStore.resolve_active()
    assert length(KeyStore.list()) == 1
  end

  test "changed remembered key fails without changing defaults or creating identities", ctx do
    previous = Identity.generate() |> Identity.encode_public_key()
    :ok = RelayConfig.remember(ctx.address, previous)
    before = File.read!(RelayConfig.path())

    error =
      capture_io(:stderr, fn ->
        {result, _} = run([ctx.address], "yes\n")
        assert result == {:exit, 1}
      end)

    assert error =~ "key changed"
    assert File.read!(RelayConfig.path()) == before
    assert KeyStore.list() == []
  end

  test "a supplied key cannot overwrite a remembered pin", ctx do
    :ok = RelayConfig.remember(ctx.address, ctx.pin)
    wrong = Identity.generate() |> Identity.encode_public_key()

    capture_io(:stderr, fn ->
      {result, _} = run([ctx.address, "--relay-pubkey", wrong])
      assert result == {:exit, 1}
    end)

    assert {:ok, %{public_key: pin}} = RelayConfig.resolve()
    assert pin == ctx.pin
  end

  test "existing project identity is reused without altering the global selector", ctx do
    {:ok, global} = KeyStore.generate()
    {:ok, local} = KeyStore.generate()
    :ok = KeyStore.set_default(Identity.name(global))
    File.write!("arc.key", Identity.name(local))
    {result, output} = run([ctx.address, "--relay-pubkey", ctx.pin])
    assert result == :ok
    assert output =~ Identity.name(local)
    assert {:ok, ^global} = KeyStore.default()
    assert {:ok, ^local} = KeyStore.resolve_active()
  end

  test "an invalid explicit identity or unselected existing keys are not replaced", ctx do
    System.put_env("ARC_KEY", "missing")

    capture_io(:stderr, fn ->
      {result, _} = run([ctx.address, "--relay-pubkey", ctx.pin])
      assert result == {:exit, 1}
    end)

    System.delete_env("ARC_KEY")
    {:ok, _} = KeyStore.generate()

    capture_io(:stderr, fn ->
      {result, _} = run([ctx.address, "--relay-pubkey", ctx.pin])
      assert result == {:exit, 1}
    end)

    refute File.exists?(RelayConfig.path())
    assert length(KeyStore.list()) == 1
  end

  test "environment overrides are reported without being overwritten", ctx do
    System.put_env("ARC_RELAY", "another.example:7331")
    {result, output} = run([ctx.address, "--relay-pubkey", ctx.pin])
    assert result == :ok
    assert output =~ "environment overrides"
    assert System.get_env("ARC_RELAY") == "another.example:7331"
    assert {:ok, saved} = RelayConfig.load()
    assert saved["default"] == ctx.address
  end

  test "unreachable relay leaves setup untouched" do
    {:ok, socket} = :gen_tcp.listen(0, [:binary, active: false])
    {:ok, port} = :inet.port(socket)
    :gen_tcp.close(socket)

    capture_io(:stderr, fn ->
      {result, _} = run(["127.0.0.1:#{port}"], "yes\n")
      assert result == {:exit, 1}
    end)

    refute File.exists?(RelayConfig.path())
    assert KeyStore.list() == []
  end

  test "duplicate trust options fail before changing settings", ctx do
    capture_io(:stderr, fn ->
      {result, _} = run([ctx.address, "--relay-pubkey", ctx.pin, "--relay-pubkey", ctx.pin])
      assert result == {:exit, 1}
    end)

    refute File.exists?(RelayConfig.path())
    assert KeyStore.list() == []
  end

  test "join is reserved from installed tool aliases" do
    assert Arc.CLI.ToolRegistry.reserved_command?("join")
  end

  defp run(args, input \\ "") do
    caller = self()

    output =
      capture_io(input, fn ->
        send(caller, {:join_result, Arc.CLI.main(["join" | args])})
      end)

    assert_received {:join_result, result}
    {result, output}
  end
end
