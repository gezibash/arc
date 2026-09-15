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

    {x_pub, _} = Identity.to_x25519(id)
    %{id: id, x_hex: Base.encode16(x_pub, case: :lower)}
  end

  test "publish publishes the identity and the keyex", %{id: id, x_hex: x_hex} do
    output = capture_io(fn -> Arc.CLI.main(["publish"]) end)

    assert output =~ "keyex:      published"
    assert output =~ x_hex

    {:ok, [entry]} = Arc.Control.resolve(Identity.name(id))
    assert Base.encode16(entry.x25519_public, case: :lower) == x_hex
  end

  test "resolve prints the x25519 key", %{id: id, x_hex: x_hex} do
    capture_io(fn -> Arc.CLI.main(["publish"]) end)
    output = capture_io(fn -> Arc.CLI.main(["resolve", Identity.name(id)]) end)

    assert output =~ "x25519:     #{x_hex}"
  end

  test "resolve prints none when no keyex is published", %{id: id} do
    :ok = Arc.Control.publish(id)
    output = capture_io(fn -> Arc.CLI.main(["resolve", Identity.name(id)]) end)

    assert output =~ "keyex:      none"
    assert output =~ "x25519:     none"
  end
end
