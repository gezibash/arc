defmodule Arc.Net.RelayConfigTest do
  use ExUnit.Case, async: false

  import Bitwise

  alias Arc.Net.RelayConfig

  @pin_a String.duplicate("a", 64)
  @pin_b String.duplicate("b", 64)

  setup do
    root =
      Path.join(
        System.tmp_dir!(),
        "arc-relay-config-" <> Integer.to_string(System.unique_integer([:positive]))
      )

    path = Path.join(root, "relays.json")
    old_path = Application.get_env(:arc_net, :relay_config_path)
    old_relay = System.get_env("ARC_RELAY")
    old_pin = System.get_env("ARC_RELAY_PUBKEY")

    Application.put_env(:arc_net, :relay_config_path, path)
    System.delete_env("ARC_RELAY")
    System.delete_env("ARC_RELAY_PUBKEY")

    on_exit(fn ->
      restore_application_env(:arc_net, :relay_config_path, old_path)
      restore_system_env("ARC_RELAY", old_relay)
      restore_system_env("ARC_RELAY_PUBKEY", old_pin)
      File.rm_rf(root)
    end)

    %{root: root, path: path}
  end

  test "returns an empty document when no relay configuration exists", %{path: path} do
    assert RelayConfig.path() == path

    assert {:ok, %{"version" => 1, "default" => nil, "relays" => %{}}} = RelayConfig.load()
  end

  test "normalizes a JSON null default to nil", %{path: path} do
    File.mkdir_p!(Path.dirname(path))
    File.write!(path, ~s({"version":1,"default":null,"relays":{}}))
    File.chmod!(path, 0o600)

    assert {:ok, %{"version" => 1, "default" => nil, "relays" => %{}}} = RelayConfig.load()
  end

  test "normalizes supported relay addresses" do
    assert {:ok, "relay.example.com:7331"} = RelayConfig.normalize_address("Relay.Example.Com")
    assert {:ok, "127.0.0.1:7332"} = RelayConfig.normalize_address("127.0.0.1:7332")
    assert {:ok, "[::1]:7331"} = RelayConfig.normalize_address("[::1]")
    assert {:ok, "[::1]:7332"} = RelayConfig.normalize_address("[::1]:7332")
    assert {~c"::1", 7332} = Arc.Net.relay_address_from("[::1]:7332")
  end

  test "rejects URI syntax, malformed hosts, and invalid ports" do
    for address <- [
          "https://relay.example.com",
          "relay.example.com/path",
          "relay.example.com:0",
          "relay.example.com:65536",
          "relay.example.com:not-a-port",
          "::1",
          "relay example.com",
          "relay.example.com\n"
        ] do
      assert {:error, :invalid_relay_address} = RelayConfig.normalize_address(address)
    end
  end

  test "remembers a canonical relay and rejects a changed pin", %{path: path} do
    assert :ok = RelayConfig.remember("Relay.Example.Com", String.upcase(@pin_a))

    assert {:ok,
            %{
              "version" => 1,
              "default" => "relay.example.com:7331",
              "relays" => %{"relay.example.com:7331" => @pin_a}
            }} = RelayConfig.load()

    assert {:ok, %{type: :regular, mode: mode}} = File.lstat(path)
    assert (mode &&& 0o077) == 0
    assert {:error, :relay_pubkey_mismatch} = RelayConfig.remember("relay.example.com", @pin_b)
  end

  test "accepts existing relay pin forms and persists canonical hex" do
    binary_pin = :binary.copy(<<0xAA>>, 32)
    base64_pin = Base.encode64(binary_pin)
    hex_pin = Base.encode16(binary_pin, case: :lower)

    assert :ok = RelayConfig.remember("relay.example.com", base64_pin)
    assert {:ok, %{public_key: ^hex_pin}} = RelayConfig.resolve()

    assert {:ok, %{address: "other.example.com:7331", public_key: ^hex_pin}} =
             RelayConfig.resolve(relay: "other.example.com", relay_pubkey: "0x" <> hex_pin)
  end

  test "fails closed for a symlink, oversized file, malformed document, and loose permissions", %{
    root: root,
    path: path
  } do
    File.mkdir_p!(root)
    target = Path.join(root, "target.json")
    File.write!(target, "{}")
    File.ln_s!(target, path)
    assert {:error, :invalid_relay_config} = RelayConfig.load()

    File.rm!(path)
    File.write!(path, String.duplicate("x", 16_385))
    assert {:error, :invalid_relay_config} = RelayConfig.load()

    File.write!(path, "{not-json")
    File.chmod!(path, 0o600)
    assert {:error, :invalid_relay_config} = RelayConfig.load()

    File.write!(path, :json.encode(%{"version" => 1, "default" => nil, "relays" => %{}}))
    File.chmod!(path, 0o644)
    assert {:error, :invalid_relay_config} = RelayConfig.load()
  end

  test "resolves explicit options without reading a broken saved configuration", %{path: path} do
    File.mkdir_p!(Path.dirname(path))
    File.write!(path, "broken")
    File.chmod!(path, 0o600)

    assert {:ok, %{address: "relay.example.com:7331", public_key: @pin_a}} =
             RelayConfig.resolve(relay: "Relay.Example.Com", relay_pubkey: @pin_a)
  end

  test "uses an exact remembered address pin and never borrows one for another address" do
    assert :ok = RelayConfig.remember("saved.example.com", @pin_a)

    System.put_env("ARC_RELAY", "environment.example.com:7332")

    assert {:ok, %{address: "environment.example.com:7332", public_key: nil}} =
             RelayConfig.resolve()

    System.put_env("ARC_RELAY_PUBKEY", @pin_b)

    assert {:ok, %{address: "environment.example.com:7332", public_key: @pin_b}} =
             RelayConfig.resolve()

    System.delete_env("ARC_RELAY_PUBKEY")
    assert :ok = RelayConfig.remember("environment.example.com:7332", @pin_b)

    assert {:ok, %{address: "environment.example.com:7332", public_key: @pin_b}} =
             RelayConfig.resolve()
  end

  test "uses a saved default only with its matching pin" do
    assert :ok = RelayConfig.remember("saved.example.com", @pin_a)

    assert {:ok, %{address: "saved.example.com:7331", public_key: @pin_a}} = RelayConfig.resolve()
  end

  test "rejects malformed explicit environment instead of falling back to saved configuration" do
    assert :ok = RelayConfig.remember("saved.example.com", @pin_a)
    System.put_env("ARC_RELAY", "https://invalid.example.com")
    assert {:error, :invalid_relay_config} = RelayConfig.resolve()

    System.delete_env("ARC_RELAY")
    System.put_env("ARC_RELAY_PUBKEY", "not-a-public-key")
    assert {:error, :invalid_relay_config} = RelayConfig.resolve()
  end

  test "rejects a non-canonical persisted address or pin", %{path: path} do
    File.mkdir_p!(Path.dirname(path))

    File.write!(
      path,
      :json.encode(%{
        "version" => 1,
        "default" => "Relay.Example.Com:7331",
        "relays" => %{"Relay.Example.Com:7331" => String.upcase(@pin_a)}
      })
    )

    File.chmod!(path, 0o600)
    assert {:error, :invalid_relay_config} = RelayConfig.load()
  end

  defp restore_application_env(app, key, nil), do: Application.delete_env(app, key)
  defp restore_application_env(app, key, value), do: Application.put_env(app, key, value)
  defp restore_system_env(name, nil), do: System.delete_env(name)
  defp restore_system_env(name, value), do: System.put_env(name, value)
end
