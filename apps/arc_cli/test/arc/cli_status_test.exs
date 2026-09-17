defmodule Arc.CLI.StatusTest do
  use ExUnit.Case, async: false
  import ExUnit.CaptureIO
  alias Arc.Identity
  alias Arc.Net.Relay

  setup do
    for key <- ["ARC_KEY", "ARC_RELAY", "ARC_RELAY_PUBKEY"] do
      previous = System.get_env(key)
      System.delete_env(key)

      on_exit(fn ->
        if previous, do: System.put_env(key, previous), else: System.delete_env(key)
      end)
    end

    :ok
  end

  test "missing relay fails with valid JSON and no deployment or host snapshot" do
    {result, report} = report()
    assert result == {:exit, 1}
    assert report["state"] == "error"
    assert report["message"] =~ "ARC_RELAY"
    refute Map.has_key?(report, "docker")
    refute Map.has_key?(report, "host")
    refute Map.has_key?(report, "identity")
  end

  test "default query reports relay-owned state without requiring a selected identity" do
    {port, pin} = relay()
    System.put_env("ARC_KEY", "nonexistent-status-citizen")
    System.put_env("ARC_RELAY", "127.0.0.1:#{port}")
    System.put_env("ARC_RELAY_PUBKEY", pin)
    {result, status} = report()
    assert result == nil
    assert status["state"] == "running"
    assert status["role"] == "relay"
    assert status["public_key"] == String.downcase(pin)
    assert is_integer(status["uptime_seconds"])
    assert status["uptime_seconds"] >= 0
    assert status["version"] == to_string(Application.spec(:arc_net, :vsn))
    assert status["federation_transit"] == false
    refute Map.has_key?(status, "connected")
    output = capture_io(fn -> Arc.CLI.main(["status"]) end)
    assert output =~ "Key"
    assert output =~ "Value"
    assert output =~ "running"
    refute output =~ "connected"
  end

  test "explicit relay options override invalid environment and check is a compatibility alias" do
    {port, pin} = relay()
    System.put_env("ARC_RELAY", "invalid")
    System.put_env("ARC_RELAY_PUBKEY", "invalid")
    {result, status} = report(["--check", "--relay", "127.0.0.1:#{port}", "--relay-pubkey", pin])
    assert result == nil
    assert status["address"] == "127.0.0.1:#{port}"
    assert status["state"] == "running"
  end

  test "invalid settings fail without echoing supplied values" do
    for args <- [
          ["--relay", "sensitive-value"],
          ["--relay", "localhost:7331"],
          ["--relay", "localhost:7331", "--relay-pubkey", "sensitive-value"]
        ] do
      {result, status} = report(args)
      assert result == {:exit, 1}
      assert status["state"] == "error"
      refute :json.encode(status) |> IO.iodata_to_binary() =~ "sensitive-value"
    end
  end

  test "wrong relay pin returns a failed status" do
    {port, _} = relay()
    pin = Identity.generate() |> Identity.encode_public_key()
    {result, status} = report(["--relay", "127.0.0.1:#{port}", "--relay-pubkey", pin])
    assert result == {:exit, 1}
    assert status["state"] == "error"
  end

  test "format json supports failed diagnostics" do
    {result, output} = with_io(fn -> Arc.CLI.main(["status", "--format", "json"]) end)
    assert result == {:exit, 1}
    assert :json.decode(output)["state"] == "error"
  end

  test "help needs no service" do
    {result, output} = with_io(fn -> Arc.CLI.main(["status", "--help"]) end)
    assert result == :ok
    assert output =~ "usage: arc status"
  end

  test "invalid options fail" do
    for args <- [
          ["--relay"],
          ["extra"],
          ["--chek"],
          ["--docker-project", "arc-local"],
          ["--docker"],
          ["--socket", "/tmp/host.sock"],
          ["--format", "xml"],
          ["--json", "--format", "table"]
        ] do
      {result, stderr} = with_io(:stderr, fn -> Arc.CLI.main(["status" | args]) end)
      assert result == {:exit, 1}
      assert stderr =~ "usage: arc status"
    end
  end

  defp relay do
    relay = start_supervised!({Relay, 0})
    {Relay.get_port(relay), Base.encode16(Relay.get_pubkey(relay))}
  end

  defp report(args \\ []) do
    {result, output} = with_io(fn -> Arc.CLI.main(["status", "--json" | args]) end)
    {result, :json.decode(output)}
  end
end
