defmodule Arc.Identity.KeyStoreTest do
  use ExUnit.Case, async: false

  alias Arc.Identity
  alias Arc.Identity.KeyStore

  # Override the keys dir and default file for testing
  # We'll use a temp directory and env vars to isolate
  setup do
    tmp = Path.join(System.tmp_dir!(), "arc_ks_test_#{System.unique_integer([:positive])}")
    keys_dir = Path.join(tmp, "keys")
    default_file = Path.join(tmp, "default_key")
    File.mkdir_p!(keys_dir)

    # We'll test by saving/loading from the real config dir,
    # so let's use a process-based approach instead.
    # Actually, KeyStore uses hardcoded paths. Let's test by
    # temporarily setting up and tearing down ~/.config/arc/keys/
    # OR we can refactor to accept a config. For now, let's test
    # the module directly using real paths but clean up after.

    # Use a unique test prefix to avoid collisions
    on_exit(fn ->
      File.rm_rf!(tmp)
      System.delete_env("ARC_KEY")
    end)

    {:ok, tmp: tmp, keys_dir: keys_dir, default_file: default_file}
  end

  # These tests work against the real KeyStore paths (~/.config/arc/keys/)
  # Each test generates unique keys so they don't collide

  describe "generate and list" do
    test "generate creates a key and list includes it" do
      {:ok, id} = KeyStore.generate()
      name = Identity.name(id)

      keys = KeyStore.list()
      names = Enum.map(keys, &elem(&1, 0))
      assert name in names

      # Cleanup
      KeyStore.remove(name)
    end

    test "generate produces valid identity" do
      {:ok, id} = KeyStore.generate()
      assert %Identity{} = id
      assert byte_size(id.public_key) == 32

      KeyStore.remove(Identity.name(id))
    end
  end

  describe "get" do
    test "get by full name" do
      {:ok, id} = KeyStore.generate()
      name = Identity.name(id)

      {:ok, found} = KeyStore.get(name)
      assert found.public_key == id.public_key

      KeyStore.remove(name)
    end

    test "get by prefix" do
      {:ok, id} = KeyStore.generate()
      name = Identity.name(id)
      short = Identity.short_name(id)

      {:ok, found} = KeyStore.get(short)
      assert found.public_key == id.public_key

      KeyStore.remove(name)
    end

    test "get unknown returns not_found" do
      assert {:error, :not_found} = KeyStore.get("nonexistent-key-00000000")
    end
  end

  describe "remove" do
    test "remove deletes the key" do
      {:ok, id} = KeyStore.generate()
      name = Identity.name(id)

      :ok = KeyStore.remove(name)
      assert {:error, :not_found} = KeyStore.get(name)
    end

    test "remove clears default if it was the removed key" do
      {:ok, id} = KeyStore.generate()
      name = Identity.name(id)
      :ok = KeyStore.set_default(name)

      :ok = KeyStore.remove(name)
      assert {:error, _} = KeyStore.default()
    end
  end

  describe "default key" do
    test "set and get default" do
      {:ok, id} = KeyStore.generate()
      name = Identity.name(id)

      :ok = KeyStore.set_default(name)
      {:ok, default_id} = KeyStore.default()
      assert default_id.public_key == id.public_key

      KeyStore.remove(name)
    end

    test "no default returns error" do
      # Save current default
      prev = KeyStore.default_name()

      # Remove default file temporarily
      default_path = Path.expand(Application.fetch_env!(:arc_identity, :default_file))
      had_default = File.exists?(default_path)
      if had_default, do: File.rename!(default_path, default_path <> ".bak")

      try do
        assert {:error, :no_default} = KeyStore.default_name()
      after
        if had_default do
          File.rename!(default_path <> ".bak", default_path)
        else
          case prev do
            {:ok, _} -> :ok
            _ -> :ok
          end
        end
      end
    end
  end

  describe "resolve_active with ARC_KEY" do
    test "ARC_KEY selects the active key" do
      {:ok, id} = KeyStore.generate()
      name = Identity.name(id)
      System.put_env("ARC_KEY", name)

      {:ok, active} = KeyStore.resolve_active()
      assert active.public_key == id.public_key

      System.delete_env("ARC_KEY")
      KeyStore.remove(name)
    end
  end
end
