defmodule Arc.Data.CapabilityPackageTest do
  use ExUnit.Case, async: true

  alias Arc.Data.CapabilityPackage
  alias Arc.Identity

  test "loads, signs, and verifies a provider package fixture" do
    path = Path.expand("../../../../../test/fixtures/providers/hello-provider.json", __DIR__)
    identity = Identity.generate()

    assert {:ok, package} = CapabilityPackage.load_file(path)
    assert package["package_version"] == 1
    assert package["release"]["version"] == "1.0.0"
    assert package["release"]["channel"] == "stable"
    assert package["capability"]["scheme"] == "hello"
    assert get_in(package, ["capability", "interfaces", "cli", "namespace"]) == "hello"

    signed = CapabilityPackage.sign(identity, package)

    assert signed["provider"]["public_key"] == Identity.encode_public_key(identity)
    assert is_binary(signed["package_hash"])
    assert is_binary(get_in(signed, ["signature", "value"]))
    assert {:ok, verified} = CapabilityPackage.verify(signed)
    assert verified["package_hash"] == signed["package_hash"]
    assert verified["capability"]["title"] == "Hello Service"
  end

  test "verification fails when a signed package is tampered with" do
    path = Path.expand("../../../../../test/fixtures/providers/hello-provider.json", __DIR__)
    identity = Identity.generate()

    assert {:ok, package} = CapabilityPackage.load_file(path)

    tampered =
      identity
      |> CapabilityPackage.sign(package)
      |> put_in(["capability", "summary"], "tampered")

    assert {:error, :hash_mismatch} = CapabilityPackage.verify(tampered)
  end

  test "loads stream invocation metadata and command-level invoke overrides" do
    path = Path.expand("../../../../../test/fixtures/providers/sandbox-provider.json", __DIR__)

    assert {:ok, package} = CapabilityPackage.load_file(path)
    assert get_in(package, ["capability", "invocation", "mode"]) == "request_reply"
    assert get_in(package, ["capability", "invocation", "stream", "tty"]) == true

    [new_command, shell_command] =
      get_in(package, ["capability", "interfaces", "cli", "commands"])

    assert new_command["path"] == ["new"]
    assert shell_command["path"] == ["shell"]
    assert get_in(shell_command, ["invoke", "mode"]) == "stream"
    assert get_in(shell_command, ["invoke", "stream", "tty"]) == true
  end
end
