defmodule Arc.CLI.KeysTest do
  use ExUnit.Case, async: false

  import ExUnit.CaptureIO

  alias Arc.Identity
  alias Arc.Identity.KeyStore

  setup do
    root =
      Path.join(
        System.tmp_dir!(),
        "arc-cli-keys-#{System.pid()}-#{System.unique_integer([:positive])}"
      )

    project = Path.join(root, "project")
    File.mkdir_p!(project)
    previous_cwd = File.cwd!()
    previous_key = System.get_env("ARC_KEY")
    System.delete_env("ARC_KEY")

    for {app, key, path} <- [
          {:arc_identity, :keys_dir, "keys"},
          {:arc_identity, :default_file, "default.key"},
          {:arc_control, :control_dir, "control"}
        ] do
      previous = Application.fetch_env(app, key)
      Application.put_env(app, key, Path.join(root, path))

      on_exit(fn ->
        case previous do
          {:ok, value} -> Application.put_env(app, key, value)
          :error -> Application.delete_env(app, key)
        end
      end)
    end

    File.cd!(project)

    on_exit(fn ->
      File.cd!(previous_cwd)

      if previous_key,
        do: System.put_env("ARC_KEY", previous_key),
        else: System.delete_env("ARC_KEY")

      File.rm_rf!(root)
    end)

    %{root: root, project: File.cwd!()}
  end

  test "show and list report the same effective identity and source", ctx do
    local = saved_identity()
    global = saved_identity()
    :ok = KeyStore.set_default(Identity.name(global))
    local_path = Path.join(ctx.project, "arc.key")
    File.write!(local_path, Identity.short_name(local) <> "\n")

    assert_identity(local, local_path)
    System.put_env("ARC_KEY", Identity.name(global))
    assert_identity(global, "ARC_KEY")
    System.delete_env("ARC_KEY")
    File.rm!(local_path)
    assert_identity(global, Path.join(ctx.root, "default.key"))
  end

  test "keys use saves a global default without changing the local selection", ctx do
    local = saved_identity()
    global = saved_identity()
    File.write!("arc.key", Identity.name(local))
    File.write!(Path.join(ctx.root, "default_key"), Identity.name(local))

    output = run(["keys", "use", Identity.name(global)])
    assert output =~ "Default key: #{Identity.name(global)}"
    refute output =~ "Active key:"
    assert File.read!(Path.join(ctx.root, "default.key")) == Identity.name(global)
    refute File.exists?(Path.join(ctx.root, "default_key"))
    assert File.read!("arc.key") == Identity.name(local)
    assert_identity(local, Path.join(ctx.project, "arc.key"))
  end

  test "generating the first identity saves the new global default", ctx do
    run(["keys", "gen"])
    assert {:ok, name} = KeyStore.default_name()
    assert File.read!(Path.join(ctx.root, "default.key")) == name
    refute File.exists?(Path.join(ctx.root, "default_key"))
    assert run(["keys", "show"]) =~ name
  end

  test "publishing uses the current-directory identity" do
    local = saved_identity()
    global = saved_identity()
    :ok = KeyStore.set_default(Identity.name(global))
    File.write!("arc.key", Identity.name(local))

    output = run(["publish"])
    assert output =~ Identity.encode_public_key(local)
    refute output =~ Identity.encode_public_key(global)
  end

  test "commands reject an empty local selector before using the global identity", ctx do
    global = saved_identity()
    :ok = KeyStore.set_default(Identity.name(global))
    File.write!("arc.key", " \n")

    for args <- [["keys", "show"], ["keys", "ls"], ["publish"], ["discover"], ["tool", "ls"]] do
      {result, stderr} = with_io(:stderr, fn -> Arc.CLI.main(args) end)
      assert result == {:exit, 1}
      assert stderr =~ "Invalid identity selector"
      assert stderr =~ Path.join(ctx.project, "arc.key")
      refute stderr =~ "ARC_KEY=''"
    end
  end

  test "a non-readable local selector produces a useful error", ctx do
    File.mkdir!("arc.key")
    {result, stderr} = with_io(:stderr, fn -> Arc.CLI.main(["keys", "show"]) end)
    assert result == {:exit, 1}
    assert stderr =~ "Could not access identity selector"
    assert stderr =~ Path.join(ctx.project, "arc.key")
  end

  test "missing selection explains how to use an existing identity" do
    saved_identity()
    {result, stderr} = with_io(:stderr, fn -> Arc.CLI.main(["keys", "show"]) end)
    assert result == {:exit, 1}
    assert stderr =~ "arc keys use <name>"
    assert stderr =~ "arc keys gen"
  end

  test "an empty key store does not hide an explicit invalid selection" do
    File.write!("arc.key", "")
    {result, stderr} = with_io(:stderr, fn -> Arc.CLI.main(["keys", "ls"]) end)
    assert result == {:exit, 1}
    assert stderr =~ "Invalid identity selector"
  end

  test "generation reports a saved default when legacy cleanup fails", ctx do
    File.mkdir!(Path.join(ctx.root, "default_key"))

    capture_io(fn ->
      {result, stderr} = with_io(:stderr, fn -> Arc.CLI.main(["keys", "gen"]) end)
      assert result == {:exit, 1}
      assert stderr =~ "Default key was saved, but legacy selector cleanup failed"
      assert stderr =~ Path.join(ctx.root, "default_key")
    end)

    assert [{name, _identity}] = KeyStore.list()
    assert File.read!(Path.join(ctx.root, "default.key")) == name
    assert {:ok, _identity} = KeyStore.resolve_active()
  end

  test "keys use reports partial success when legacy cleanup fails", ctx do
    id = saved_identity()
    File.mkdir!(Path.join(ctx.root, "default_key"))

    {result, stderr} =
      with_io(:stderr, fn -> Arc.CLI.main(["keys", "use", Identity.name(id)]) end)

    assert result == {:exit, 1}
    assert stderr =~ "Default key was saved, but legacy selector cleanup failed"
    assert {:ok, ^id} = KeyStore.resolve_active()
  end

  test "removal reports partial success if default cleanup fails", ctx do
    id = saved_identity()
    :ok = KeyStore.set_default(Identity.name(id))
    File.mkdir!(Path.join(ctx.root, "default_key"))

    {result, stderr} = with_io(:stderr, fn -> Arc.CLI.main(["keys", "rm", Identity.name(id)]) end)
    assert result == {:exit, 1}
    assert stderr =~ "Key was removed, but default selector cleanup failed"
    assert {:error, :not_found} = KeyStore.get(Identity.name(id))
    refute File.exists?(Path.join(ctx.root, "default.key"))
  end

  defp assert_identity(identity, source) do
    shown = run(["keys", "show"])
    assert shown =~ "name:       #{Identity.name(identity)}"
    assert shown =~ "source:     #{source}"
    listed = run(["keys", "ls"])
    assert listed =~ "* #{Identity.name(identity)}"
    assert listed =~ "active via #{source}"
  end

  defp run(args), do: capture_io(fn -> Arc.CLI.main(args) end)

  defp saved_identity do
    {:ok, identity} = KeyStore.generate()
    identity
  end
end
