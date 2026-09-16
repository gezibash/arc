defmodule Arc.Identity.IdentitySelectionTest do
  use ExUnit.Case, async: false

  alias Arc.Identity
  alias Arc.Identity.KeyStore

  setup do
    root =
      Path.join(
        System.tmp_dir!(),
        "arc-selection-#{System.pid()}-#{System.unique_integer([:positive])}"
      )

    project = Path.join(root, "project")
    File.mkdir_p!(project)
    previous_config = Application.get_all_env(:arc_identity)
    previous_key = System.get_env("ARC_KEY")
    previous_cwd = File.cwd!()
    Application.put_env(:arc_identity, :keys_dir, Path.join(root, "keys"))
    Application.put_env(:arc_identity, :default_file, Path.join(root, "default.key"))
    System.delete_env("ARC_KEY")
    File.cd!(project)

    on_exit(fn ->
      File.cd!(previous_cwd)

      for key <- [:keys_dir, :default_file] do
        case Keyword.fetch(previous_config, key) do
          {:ok, value} -> Application.put_env(:arc_identity, key, value)
          :error -> Application.delete_env(:arc_identity, key)
        end
      end

      if previous_key,
        do: System.put_env("ARC_KEY", previous_key),
        else: System.delete_env("ARC_KEY")

      File.rm_rf!(root)
    end)

    %{root: root, project: project, default: Path.join(root, "default.key")}
  end

  test "environment wins over local and global selectors" do
    env_id = saved_identity()
    local_id = saved_identity()
    global_id = saved_identity()
    :ok = KeyStore.set_default(Identity.name(global_id))
    File.write!("arc.key", Identity.name(local_id))
    System.put_env("ARC_KEY", Identity.name(env_id))

    assert {:ok, ^env_id, :environment} = KeyStore.resolve_active_with_source()
    assert {:ok, ^env_id} = KeyStore.resolve_active()
  end

  test "local wins over global and accepts whitespace and name prefixes" do
    local_id = saved_identity()
    global_id = saved_identity()
    :ok = KeyStore.set_default(Identity.name(global_id))
    File.write!("arc.key", " \t#{Identity.short_name(local_id)}\r\n")
    path = Path.expand("arc.key")

    assert {:ok, ^local_id, {:file, ^path}} = KeyStore.resolve_active_with_source()
  end

  test "global default is used when environment and local file are missing", ctx do
    id = saved_identity()
    File.write!(ctx.default, Identity.name(id) <> "\n")
    path = ctx.default

    assert {:ok, ^id, {:file, ^path}} = KeyStore.resolve_active_with_source()
  end

  test "all selectors missing returns no_default" do
    assert {:error, :no_default} = KeyStore.resolve_active()
  end

  test "parent folders are not searched and selection follows the actual current folder", ctx do
    local_id = saved_identity()
    global_id = saved_identity()
    :ok = KeyStore.set_default(Identity.name(global_id))
    File.write!("arc.key", Identity.name(local_id))
    child = Path.join(ctx.project, "child")
    File.mkdir_p!(child)

    assert {:ok, ^local_id} = KeyStore.resolve_active()
    File.cd!(child)
    assert {:ok, ^global_id} = KeyStore.resolve_active()
    File.cd!(ctx.project)
    assert {:ok, ^local_id} = KeyStore.resolve_active()
  end

  test "unknown or empty environment selection cannot fall through" do
    id = saved_identity()
    :ok = KeyStore.set_default(Identity.name(id))
    File.write!("arc.key", Identity.name(id))
    System.put_env("ARC_KEY", "nonexistent-key")
    assert {:error, :not_found} = KeyStore.resolve_active()

    System.put_env("ARC_KEY", " \t")
    assert {:error, {:invalid_identity_selector, :environment}} = KeyStore.resolve_active()
  end

  test "unknown, empty, multiline and invalid UTF-8 local selectors cannot fall through" do
    id = saved_identity()
    :ok = KeyStore.set_default(Identity.name(id))
    path = Path.expand("arc.key")
    File.write!(path, "nonexistent-key")
    assert {:error, :not_found} = KeyStore.resolve_active()

    for contents <- ["", " \n\t", "#{Identity.name(id)}\n#{Identity.name(id)}", <<255>>] do
      File.write!(path, contents)
      assert {:error, {:invalid_identity_selector, {:file, ^path}}} = KeyStore.resolve_active()
    end
  end

  test "unreadable local selectors cannot fall through" do
    id = saved_identity()
    :ok = KeyStore.set_default(Identity.name(id))
    path = Path.expand("arc.key")
    File.mkdir!(path)

    assert {:error, {:identity_selector_file, ^path, :eisdir}} = KeyStore.resolve_active()
  end

  test "dangling local symlinks cannot fall through" do
    id = saved_identity()
    :ok = KeyStore.set_default(Identity.name(id))
    path = Path.expand("arc.key")
    File.ln_s!("missing-selector", path)

    assert {:error, {:identity_selector_file, ^path, :enoent}} = KeyStore.resolve_active()
  end

  test "ambiguous local and environment selections cannot fall through" do
    {first, second, prefix} = identities_with_shared_prefix()
    :ok = KeyStore.save(first)
    :ok = KeyStore.save(second)
    :ok = KeyStore.set_default(Identity.name(first))
    File.write!("arc.key", prefix)
    assert {:error, :ambiguous} = KeyStore.resolve_active()

    File.write!("arc.key", Identity.name(first))
    System.put_env("ARC_KEY", prefix)
    assert {:error, :ambiguous} = KeyStore.resolve_active()
  end

  test "legacy default is read only when the new selector is absent", ctx do
    old_id = saved_identity()
    new_id = saved_identity()
    legacy = Path.join(ctx.root, "default_key")
    File.write!(legacy, Identity.name(old_id))
    assert {:ok, ^old_id, {:file, ^legacy}} = KeyStore.resolve_active_with_source()

    File.write!(ctx.default, Identity.name(new_id))
    assert {:ok, ^new_id} = KeyStore.resolve_active()
  end

  test "saving a default writes the new file and retires the legacy selector", ctx do
    old_id = saved_identity()
    new_id = saved_identity()
    legacy = Path.join(ctx.root, "default_key")
    File.write!(legacy, Identity.name(old_id))

    assert :ok = KeyStore.set_default(Identity.name(new_id))
    assert File.read!(ctx.default) == Identity.name(new_id)
    refute File.exists?(legacy)
    assert {:ok, ^new_id} = KeyStore.default()
  end

  test "a failed default write preserves the old selector", ctx do
    id = saved_identity()
    legacy = Path.join(ctx.root, "default_key")
    File.write!(legacy, Identity.name(id))
    File.mkdir!(ctx.default)

    assert {:error, :eisdir} = KeyStore.set_default(Identity.name(id))
    assert File.read!(legacy) == Identity.name(id)
  end

  test "invalid or unreadable new default cannot fall through to legacy", ctx do
    id = saved_identity()
    File.write!(Path.join(ctx.root, "default_key"), Identity.name(id))
    File.write!(ctx.default, "nonexistent-key")
    assert {:error, :not_found} = KeyStore.resolve_active()

    File.write!(ctx.default, "")
    path = ctx.default
    assert {:error, {:invalid_identity_selector, {:file, ^path}}} = KeyStore.resolve_active()

    File.rm!(ctx.default)
    File.mkdir!(ctx.default)
    assert {:error, {:identity_selector_file, ^path, :eisdir}} = KeyStore.resolve_active()
  end

  test "removing a global default does not resurrect a legacy identity", ctx do
    old_id = saved_identity()
    new_id = saved_identity()
    legacy = Path.join(ctx.root, "default_key")
    File.write!(legacy, Identity.name(old_id))
    File.write!(ctx.default, Identity.name(new_id))

    assert :ok = KeyStore.remove(Identity.name(new_id))
    assert {:error, :no_default} = KeyStore.default()
    refute File.exists?(legacy)
    assert {:ok, ^old_id} = KeyStore.get(Identity.name(old_id))
  end

  test "removing a legacy default clears its selector", ctx do
    id = saved_identity()
    File.write!(Path.join(ctx.root, "default_key"), Identity.short_name(id))

    assert :ok = KeyStore.remove(Identity.name(id))
    assert {:error, :no_default} = KeyStore.default()
  end

  defp saved_identity do
    {:ok, id} = KeyStore.generate()
    id
  end

  defp identities_with_shared_prefix do
    Stream.repeatedly(&Identity.generate/0)
    |> Enum.reduce_while(%{}, fn id, seen ->
      prefix = String.first(Identity.name(id))

      case Map.fetch(seen, prefix) do
        {:ok, previous} -> {:halt, {previous, id, prefix}}
        :error -> {:cont, Map.put(seen, prefix, id)}
      end
    end)
  end
end
