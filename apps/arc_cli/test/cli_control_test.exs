defmodule Arc.CLI.ControlTest do
  use ExUnit.Case, async: false

  import ExUnit.CaptureIO

  alias Arc.Identity
  alias Arc.Identity.KeyStore

  setup do
    Arc.Control.Local.reset()
    id = Identity.generate()
    :ok = KeyStore.save(id)
    System.put_env("ARC_KEY", Identity.name(id))

    on_exit(fn ->
      System.delete_env("ARC_KEY")
      KeyStore.remove(Identity.name(id))
    end)

    %{id: id}
  end

  test "publish publishes the identity", %{id: id} do
    output = capture_io(fn -> Arc.CLI.main(["publish"]) end)

    assert output =~ "public_key: #{Identity.encode_public_key(id)}"
    refute output =~ "keyex"
    assert {:ok, [_entry]} = Arc.Control.resolve(Identity.name(id))
  end

  test "resolve prints the public key and no key exchange", %{id: id} do
    capture_io(fn -> Arc.CLI.main(["publish"]) end)
    output = capture_io(fn -> Arc.CLI.main(["resolve", Identity.name(id)]) end)

    assert output =~ "public_key: #{Identity.encode_public_key(id)}"
    refute output =~ "x25519"
  end
end
