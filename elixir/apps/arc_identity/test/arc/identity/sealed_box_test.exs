defmodule Arc.Identity.SealedBoxTest do
  use ExUnit.Case, async: true

  alias Arc.Identity
  alias Arc.Identity.SealedBox

  defp x_pub(identity) do
    {pub, _} = Identity.to_x25519(identity)
    pub
  end

  describe "seal/2 and open/2" do
    setup do
      %{bob: Identity.generate(), carol: Identity.generate()}
    end

    test "round trips an empty body", %{bob: bob} do
      assert {:ok, ""} = SealedBox.open(bob, SealedBox.seal(x_pub(bob), ""))
    end

    test "round trips one byte", %{bob: bob} do
      assert {:ok, "x"} = SealedBox.open(bob, SealedBox.seal(x_pub(bob), "x"))
    end

    test "round trips non-ASCII text", %{bob: bob} do
      msg = "range #23–#27, ü, 日本"
      assert {:ok, ^msg} = SealedBox.open(bob, SealedBox.seal(x_pub(bob), msg))
    end

    test "round trips 1 MiB", %{bob: bob} do
      msg = :crypto.strong_rand_bytes(1_048_576)
      assert {:ok, ^msg} = SealedBox.open(bob, SealedBox.seal(x_pub(bob), msg))
    end

    test "output is version byte, 32 byte ephemeral key, ciphertext, 16 byte tag", %{bob: bob} do
      sealed = SealedBox.seal(x_pub(bob), "hello")
      assert <<1, _ek::binary-size(32), rest::binary>> = sealed
      assert byte_size(rest) == 5 + 16
    end

    test "wrong recipient cannot open", %{bob: bob, carol: carol} do
      assert {:error, :open_failed} = SealedBox.open(carol, SealedBox.seal(x_pub(bob), "secret"))
    end

    test "any flipped byte fails to open", %{bob: bob} do
      sealed = SealedBox.seal(x_pub(bob), "secret")

      for i <- 0..(byte_size(sealed) - 1) do
        <<head::binary-size(i), byte, tail::binary>> = sealed
        tampered = <<head::binary, Bitwise.bxor(byte, 0x01), tail::binary>>
        assert {:error, :open_failed} = SealedBox.open(bob, tampered), "byte #{i}"
      end
    end

    test "two seals of the same plaintext differ", %{bob: bob} do
      refute SealedBox.seal(x_pub(bob), "same") == SealedBox.seal(x_pub(bob), "same")
    end

    test "unknown version fails to open", %{bob: bob} do
      <<1, rest::binary>> = SealedBox.seal(x_pub(bob), "hello")
      assert {:error, :open_failed} = SealedBox.open(bob, <<2, rest::binary>>)
    end

    test "truncated input fails to open", %{bob: bob} do
      assert {:error, :open_failed} = SealedBox.open(bob, <<1, 0::256>>)
      assert {:error, :open_failed} = SealedBox.open(bob, "")
    end
  end
end
