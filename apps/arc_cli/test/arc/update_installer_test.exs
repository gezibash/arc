defmodule Arc.CLI.Update.InstallerTest do
  use ExUnit.Case, async: false

  alias Arc.CLI.Update.Installer

  setup do
    root =
      Path.join(System.tmp_dir!(), "arc-update-installer-#{System.unique_integer([:positive])}")

    install_root = Path.join([root, "share", "arc"])
    File.mkdir_p!(Path.dirname(install_root))
    build_release(install_root, "0.4.1")

    on_exit(fn -> File.rm_rf!(root) end)

    %{root: root, install_root: install_root, work: Path.join(root, "work")}
  end

  test "validates only a complete archive with the expected version and bytes", ctx do
    archive = build_archive(ctx.work, "9.9.9")

    assert {:ok, %{version: "9.9.9", entries: entries}} =
             Installer.validate_archive(archive.path,
               version: "9.9.9",
               sha256: archive.sha256,
               size: archive.size
             )

    # erl_tar records the regular files; directories are implied by their paths.
    assert entries == 4

    assert {:error, :archive_size_mismatch} =
             Installer.validate_archive(archive.path,
               version: "9.9.9",
               sha256: archive.sha256,
               size: archive.size + 1
             )

    assert {:error, :archive_digest_mismatch} =
             Installer.validate_archive(archive.path,
               version: "9.9.9",
               sha256: String.duplicate("0", 64),
               size: archive.size
             )

    assert {:error, :missing_release_directory} =
             Installer.validate_archive(archive.path,
               version: "9.9.8",
               sha256: archive.sha256,
               size: archive.size
             )
  end

  test "refuses archive members outside arc/ and non-regular members", ctx do
    stray = build_archive(Path.join(ctx.work, "stray"), "9.9.9", extra: {"notes.txt", "x"})

    assert {:error, {:unexpected_archive_path, "notes.txt"}} =
             Installer.validate_archive(stray.path,
               version: "9.9.9",
               sha256: stray.sha256,
               size: stray.size
             )

    linked = build_archive(Path.join(ctx.work, "linked"), "9.9.9", symlink: true)

    assert {:error, {:unsafe_archive_entry, "arc/bin/link"}} =
             Installer.validate_archive(linked.path,
               version: "9.9.9",
               sha256: linked.sha256,
               size: linked.size
             )
  end

  test "swaps the installation, keeps the previous release, and cleans staging", ctx do
    archive = build_archive(ctx.work, "9.9.9")

    assert {:ok, %{root: root, previous: previous, version: "9.9.9"}} =
             Installer.install(archive.path, ctx.install_root, version: "9.9.9")

    assert root == ctx.install_root
    assert previous == ctx.install_root <> ".previous"
    assert {:ok, "9.9.9"} = Installer.installed_version(root)
    assert {:ok, "0.4.1"} = Installer.installed_version(previous)
    assert {"arc 9.9.9 (test)\n", 0} = System.cmd(Path.join([root, "bin", "arc"]), ["version"])
    assert staging_entries(root) == []

    # A second update replaces the stale previous release rather than refusing.
    newer = build_archive(Path.join(ctx.work, "newer"), "9.9.10")
    assert {:ok, _} = Installer.install(newer.path, root, version: "9.9.10")
    assert {:ok, "9.9.10"} = Installer.installed_version(root)
    assert {:ok, "9.9.9"} = Installer.installed_version(previous)
  end

  test "leaves the installation untouched when the candidate reports another version", ctx do
    archive = build_archive(ctx.work, "9.9.9", reported_version: "1.2.3")

    assert {:error, {:candidate_reports_other_version, "arc 1.2.3 (test)"}} =
             Installer.install(archive.path, ctx.install_root, version: "9.9.9")

    assert {:ok, "0.4.1"} = Installer.installed_version(ctx.install_root)
    refute File.exists?(ctx.install_root <> ".previous")
    assert staging_entries(ctx.install_root) == []
  end

  test "refuses to remove a foreign object at the previous path", ctx do
    archive = build_archive(ctx.work, "9.9.9")
    File.mkdir_p!(ctx.install_root <> ".previous")
    File.write!(Path.join(ctx.install_root <> ".previous", "keep.txt"), "mine")

    assert {:error, :previous_path_occupied} =
             Installer.install(archive.path, ctx.install_root, version: "9.9.9")

    assert {:ok, "0.4.1"} = Installer.installed_version(ctx.install_root)
    assert File.read!(Path.join(ctx.install_root <> ".previous", "keep.txt")) == "mine"
  end

  test "rejects roots that are not release installations", ctx do
    assert {:error, :install_root_not_absolute} = Installer.verify_root("relative/arc")
    assert {:error, :install_root_invalid} = Installer.verify_root(ctx.work)
    assert :ok = Installer.verify_root(ctx.install_root)
  end

  defp staging_entries(root) do
    root
    |> Path.dirname()
    |> File.ls!()
    |> Enum.filter(&String.contains?(&1, ".update-"))
  end

  defp build_release(dir, version, opts \\ []) do
    reported = Keyword.get(opts, :reported_version, version)
    File.mkdir_p!(Path.join(dir, "bin"))
    File.mkdir_p!(Path.join([dir, "releases", version]))
    script = Path.join([dir, "bin", "arc"])
    File.write!(script, "#!/bin/sh\necho \"arc #{reported} (test)\"\n")
    File.chmod!(script, 0o755)
    File.write!(Path.join([dir, "bin", "arc_runtime"]), "#!/bin/sh\nexit 0\n")
    File.chmod!(Path.join([dir, "bin", "arc_runtime"]), 0o755)
    File.write!(Path.join([dir, "releases", "start_erl.data"]), "16.0 #{version}\n")
    File.write!(Path.join([dir, "releases", version, "arc_runtime.rel"]), "{release, test}.\n")
    dir
  end

  defp build_archive(work, version, opts \\ []) do
    File.mkdir_p!(work)
    tree = Path.join(work, "arc")
    build_release(tree, version, opts)

    if Keyword.get(opts, :symlink, false) do
      File.ln_s!("..", Path.join([tree, "bin", "link"]))
    end

    entries =
      case Keyword.get(opts, :extra) do
        {name, bytes} ->
          extra = Path.join(work, name)
          File.write!(extra, bytes)

          [
            {~c"arc", String.to_charlist(tree)},
            {String.to_charlist(name), String.to_charlist(extra)}
          ]

        nil ->
          [{~c"arc", String.to_charlist(tree)}]
      end

    path = Path.join(work, "release.tar.gz")
    :ok = :erl_tar.create(String.to_charlist(path), entries, [:compressed])
    bytes = File.read!(path)

    %{
      path: path,
      size: byte_size(bytes),
      sha256: :crypto.hash(:sha256, bytes) |> Base.encode16(case: :lower)
    }
  end
end
