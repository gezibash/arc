defmodule Arc.ControlTest do
  use ExUnit.Case, async: false

  alias Arc.Identity
  alias Arc.Control

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

  describe "publish_keyex/2" do
    test "publishes X25519 key exchange material" do
      id = Identity.generate()
      :ok = Control.publish(id)

      {x_pub, _x_priv} = Identity.to_x25519(id)
      :ok = Control.publish_keyex(id.public_key, x_pub)

      {:ok, [entry]} = Control.resolve(Identity.name(id))
      assert entry.x25519_public == x_pub
    end

    test "returns error for unknown identity" do
      fake_pk = :crypto.strong_rand_bytes(32)
      {:error, :not_found} = Control.publish_keyex(fake_pk, <<0::256>>)
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

    test "receives keyex events" do
      {:ok, _} = Control.subscribe(:keyex_published)

      id = Identity.generate()
      :ok = Control.publish(id)

      {x_pub, _} = Identity.to_x25519(id)
      :ok = Control.publish_keyex(id.public_key, x_pub)

      assert_receive {:arc_control, {:keyex_published, entry}}
      assert entry.x25519_public == x_pub
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

      # Both publish key exchange material
      {alice_x_pub, alice_x_priv} = Identity.to_x25519(alice)
      {bob_x_pub, bob_x_priv} = Identity.to_x25519(bob)

      :ok = Control.publish_keyex(alice.public_key, alice_x_pub)
      :ok = Control.publish_keyex(bob.public_key, bob_x_pub)

      # Alice resolves Bob by petname
      {:ok, [bob_entry]} = Control.resolve(Identity.name(bob))
      assert bob_entry.x25519_public == bob_x_pub

      # Bob resolves Alice by short name
      {:ok, [alice_entry]} = Control.resolve(Identity.short_name(alice))
      assert alice_entry.x25519_public == alice_x_pub

      # Both derive the same shared secret
      shared_alice = :crypto.compute_key(:ecdh, bob_entry.x25519_public, alice_x_priv, :x25519)
      shared_bob = :crypto.compute_key(:ecdh, alice_entry.x25519_public, bob_x_priv, :x25519)
      assert shared_alice == shared_bob
    end
  end
end
