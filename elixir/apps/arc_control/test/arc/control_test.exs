defmodule Arc.ControlTest do
  use ExUnit.Case, async: false

  alias Arc.Control
  alias Arc.Identity

  setup do
    Arc.Control.Local.reset()
    :ok
  end

  describe "publish/1 and resolve/1" do
    test "publish then resolve by full petname" do
      id = Identity.generate()
      :ok = Control.publish(id)

      {:ok, [entry]} = Control.resolve(Identity.name(id))
      assert entry.public_key == id.public_key
      assert entry.name == Identity.name(id)
      assert entry.status == :active
    end

    test "resolve by short petname" do
      id = Identity.generate()
      :ok = Control.publish(id)

      {:ok, [entry]} = Control.resolve(Identity.short_name(id))
      assert entry.public_key == id.public_key
    end

    test "resolve by public key hex prefix" do
      id = Identity.generate()
      :ok = Control.publish(id)

      prefix = id.public_key |> Base.encode16(case: :lower) |> String.slice(0, 8)
      {:ok, results} = Control.resolve(prefix)
      assert Enum.any?(results, &(&1.public_key == id.public_key))
    end

    test "resolve returns empty list for unknown name" do
      {:ok, []} = Control.resolve("nobody-here-00000000")
    end

    test "multiple identities resolvable" do
      id1 = Identity.generate()
      id2 = Identity.generate()
      :ok = Control.publish(id1)
      :ok = Control.publish(id2)

      {:ok, [e1]} = Control.resolve(Identity.name(id1))
      {:ok, [e2]} = Control.resolve(Identity.name(id2))
      refute e1.public_key == e2.public_key
    end
  end

  describe "revoke/1" do
    test "revoked identity no longer resolves" do
      id = Identity.generate()
      :ok = Control.publish(id)
      :ok = Control.revoke(id.public_key)

      {:ok, []} = Control.resolve(Identity.name(id))
    end

    test "returns error for unknown identity" do
      fake_pk = :crypto.strong_rand_bytes(32)
      {:error, :not_found} = Control.revoke(fake_pk)
    end
  end

  describe "subscribe/1" do
    test "receives publish events" do
      {:ok, _} = Control.subscribe(:identity_published)

      id = Identity.generate()
      :ok = Control.publish(id)

      assert_receive {:arc_control, {:identity_published, entry}}
      assert entry.public_key == id.public_key
    end

    test "receives revoke events" do
      {:ok, _} = Control.subscribe(:identity_revoked)

      id = Identity.generate()
      :ok = Control.publish(id)
      :ok = Control.revoke(id.public_key)

      assert_receive {:arc_control, {:identity_revoked, entry}}
      assert entry.status == :revoked
    end

    test ":all receives everything" do
      {:ok, _} = Control.subscribe(:all)

      id = Identity.generate()
      :ok = Control.publish(id)
      :ok = Control.revoke(id.public_key)

      assert_receive {:arc_control, {:identity_published, _}}
      assert_receive {:arc_control, {:identity_revoked, _}}
    end
  end

  describe "two-agent key exchange" do
    test "two agents resolve each other and derive shared secret" do
      alice = Identity.generate()
      bob = Identity.generate()

      :ok = Control.publish(alice)
      :ok = Control.publish(bob)

      # Each side resolves the other and derives the X25519 key from the
      # Ed25519 public key. No key exchange record is published.
      {:ok, [bob_entry]} = Control.resolve(Identity.name(bob))
      {:ok, [alice_entry]} = Control.resolve(Identity.short_name(alice))
      {:ok, bob_x_pub} = Identity.public_key_to_x25519(bob_entry.public_key)
      {:ok, alice_x_pub} = Identity.public_key_to_x25519(alice_entry.public_key)

      {_, alice_x_priv} = Identity.to_x25519(alice)
      {_, bob_x_priv} = Identity.to_x25519(bob)
      shared_alice = :crypto.compute_key(:ecdh, bob_x_pub, alice_x_priv, :x25519)
      shared_bob = :crypto.compute_key(:ecdh, alice_x_pub, bob_x_priv, :x25519)
      assert shared_alice == shared_bob
    end
  end
end
