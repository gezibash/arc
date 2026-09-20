defmodule Arc.IdentityTest do
  use ExUnit.Case, async: true

  alias Arc.Identity

  describe "generate/0" do
    test "produces a valid identity" do
      id = Identity.generate()
      assert byte_size(id.seed) == 32
      assert byte_size(id.public_key) == 32
      assert byte_size(id.secret_key) == 32
    end

    test "generates unique identities" do
      id1 = Identity.generate()
      id2 = Identity.generate()
      refute id1.public_key == id2.public_key
    end
  end

  describe "from_seed/1" do
    test "is deterministic" do
      seed = :crypto.strong_rand_bytes(32)
      id1 = Identity.from_seed(seed)
      id2 = Identity.from_seed(seed)
      assert id1.public_key == id2.public_key
      assert id1.secret_key == id2.secret_key
    end
  end

  describe "name/1 and short_name/1" do
    test "name is deterministic" do
      id = Identity.generate()
      assert Identity.name(id) == Identity.name(id)
    end

    test "name has adjective-noun-suffix format" do
      id = Identity.generate()
      name = Identity.name(id)
      parts = String.split(name, "-")
      assert length(parts) == 3
      [_adj, _noun, suffix] = parts
      assert String.length(suffix) == 8
      assert suffix =~ ~r/^[0-9a-f]{8}$/
    end

    test "short_name has adjective-noun format" do
      id = Identity.generate()
      short = Identity.short_name(id)
      parts = String.split(short, "-")
      assert length(parts) == 2
    end

    test "short_name is prefix of name" do
      id = Identity.generate()
      assert String.starts_with?(Identity.name(id), Identity.short_name(id))
    end

    test "different keys produce different names" do
      id1 = Identity.generate()
      id2 = Identity.generate()
      refute Identity.name(id1) == Identity.name(id2)
    end

    test "works with raw public key" do
      id = Identity.generate()
      assert Identity.name(id) == Identity.name(id.public_key)
      assert Identity.short_name(id) == Identity.short_name(id.public_key)
    end
  end

  describe "sign/2 and verify/3" do
    test "valid signature verifies" do
      id = Identity.generate()
      msg = "hello arc"
      sig = Identity.sign(id, msg)
      assert Identity.verify(id.public_key, msg, sig)
    end

    test "wrong message fails verification" do
      id = Identity.generate()
      sig = Identity.sign(id, "hello")
      refute Identity.verify(id.public_key, "wrong", sig)
    end

    test "wrong key fails verification" do
      id1 = Identity.generate()
      id2 = Identity.generate()
      sig = Identity.sign(id1, "hello")
      refute Identity.verify(id2.public_key, "hello", sig)
    end
  end

  describe "to_x25519/1" do
    test "derives X25519 keypair for ECDH" do
      id_a = Identity.generate()
      id_b = Identity.generate()

      {x_pub_a, x_priv_a} = Identity.to_x25519(id_a)
      {x_pub_b, x_priv_b} = Identity.to_x25519(id_b)

      shared_a = :crypto.compute_key(:ecdh, x_pub_b, x_priv_a, :x25519)
      shared_b = :crypto.compute_key(:ecdh, x_pub_a, x_priv_b, :x25519)
      assert shared_a == shared_b
    end

    # Vector from libsodium test/default/ed25519_convert.
    test "matches libsodium crypto_sign_ed25519_sk_to_curve25519" do
      id =
        Identity.from_seed(
          Base.decode16!("421151a459faeade3d247115f94aedae42318124095afabe4d1451a559faedee",
            case: :lower
          )
        )

      assert Base.encode16(id.public_key, case: :lower) ==
               "b5076a8474a832daee4dd5b4040983b6623b5f344aca57d4d6ee4baf3f259e6e"

      {x_pub, x_priv} = Identity.to_x25519(id)

      assert Base.encode16(x_pub, case: :lower) ==
               "f1814f0e8ff1043d8a44d25babff3cedcae6c22c3edaa48f857ae70de2baae50"

      assert Base.encode16(x_priv, case: :lower) ==
               "8052030376d47112be7f73ed7a019293dd12ad910b654455798b4667d73de166"
    end
  end

  describe "public_key_to_x25519/1" do
    test "matches the public half of to_x25519/1" do
      for _ <- 1..200 do
        id = Identity.generate()
        {x_pub, _} = Identity.to_x25519(id)
        assert {:ok, ^x_pub} = Identity.public_key_to_x25519(id.public_key)
      end
    end

    test "ignores the sign bit of x" do
      id = Identity.generate()
      <<head::binary-size(31), last>> = id.public_key
      flipped = <<head::binary, Bitwise.bxor(last, 0x80)>>

      assert Identity.public_key_to_x25519(flipped) ==
               Identity.public_key_to_x25519(id.public_key)
    end

    test "rejects keys that map to points of small order" do
      p = Integer.pow(2, 255) - 19

      for y <- [0, 1, p - 1] do
        assert {:error, :invalid_public_key} =
                 Identity.public_key_to_x25519(<<y::little-size(256)>>)
      end
    end

    test "rejects a y that is not below the field prime" do
      p = Integer.pow(2, 255) - 19

      assert {:error, :invalid_public_key} =
               Identity.public_key_to_x25519(<<p::little-size(256)>>)
    end

    test "rejects input that is not 32 bytes" do
      assert {:error, :invalid_public_key} = Identity.public_key_to_x25519(<<1, 2, 3>>)
    end
  end

  describe "encode_public_key/1" do
    test "encodes as lowercase hex" do
      id = Identity.generate()
      hex = Identity.encode_public_key(id)
      assert String.length(hex) == 64
      assert hex == String.downcase(hex)
      assert {:ok, id.public_key} == Base.decode16(hex, case: :lower)
    end

    test "works with raw public key" do
      id = Identity.generate()
      assert Identity.encode_public_key(id) == Identity.encode_public_key(id.public_key)
    end
  end
end
