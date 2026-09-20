defmodule Arc.CLI.Update.PackageTest do
  use ExUnit.Case, async: true

  alias Arc.CLI.Update.Package

  setup do
    root =
      Path.join(
        System.tmp_dir!(),
        "arc-update-package-" <> Integer.to_string(System.unique_integer([:positive]))
      )

    File.mkdir_p!(root)
    on_exit(fn -> File.rm_rf!(root) end)
    %{root: root}
  end

  test "accepts a bounded OTP full-release archive with matching immutable metadata", %{
    root: root
  } do
    archive = build_archive(root)
    expected = expected(archive, "2.0.0", "arc-build-2")

    assert {:ok, result} =
             Package.validate_archive(archive,
               expected: expected,
               source_version: "1.0.0"
             )

    assert result.version == "2.0.0"
    assert result.build == "arc-build-2"
    assert result.relup.upgrade_sources == ["1.0.0"]
    assert result.relup_sha256 == result.relup.sha256
  end

  test "refuses a signed archive whose declared build does not match the channel edge", %{
    root: root
  } do
    archive = build_archive(root)

    assert {:error, :build_metadata_mismatch} =
             Package.validate_archive(archive,
               expected: expected(archive, "2.0.0", "another-build"),
               source_version: "1.0.0"
             )
  end

  test "requires an exact source edge in the relup", %{root: root} do
    archive = build_archive(root)

    assert {:error, {:relup_source_missing, "1.5.0"}} =
             Package.validate_archive(archive,
               expected: expected(archive, "2.0.0", "arc-build-2"),
               source_version: "1.5.0"
             )
  end

  test "binds the signed runtime to the OTP release resource", %{root: root} do
    archive = build_archive(root, "16.4")

    assert {:error, :release_resource_mismatch} =
             Package.validate_archive(archive,
               expected: expected(archive, "2.0.0", "arc-build-2"),
               source_version: "1.0.0"
             )
  end

  test "refuses a package that would overwrite an installed application directory", %{root: root} do
    archive = build_archive(root)
    installed_root = Path.join(root, "installed")
    File.mkdir_p!(Path.join(installed_root, "lib/arc_net-2.0.0"))

    assert {:error, {:existing_library_destination, "lib/arc_net-2.0.0/ebin/arc_net.app"}} =
             Package.validate_archive(archive,
               expected: expected(archive, "2.0.0", "arc-build-2"),
               source_version: "1.0.0",
               installed_root: installed_root
             )
  end

  test "rejects restart and unapproved migration instructions" do
    restart = "{\"2.0.0\", [{\"1.0.0\", [], [restart_emulator]}], []}.\n"

    assert {:error, {:restart_or_hard_purge_instruction, :restart_emulator}} =
             Package.validate_hot_relup(restart, target_version: "2.0.0", source_version: "1.0.0")

    migration = "{\"2.0.0\", [{\"1.0.0\", [], [{apply, {arc_migration, migrate, []}}]}], []}.\n"

    assert {:error, {:unapproved_migration, {:apply, {:arc_migration, :migrate, []}}}} =
             Package.validate_hot_relup(migration,
               target_version: "2.0.0",
               source_version: "1.0.0"
             )

    assert {:ok, _} =
             Package.validate_hot_relup(migration,
               target_version: "2.0.0",
               source_version: "1.0.0",
               allowed_migrations: [{:arc_migration, :migrate, []}]
             )
  end

  test "allows only the proven relay module on the hot path" do
    protected =
      "{\"2.0.0\", [{\"1.0.0\", [], [{load_object_code, {arc_net, \"2.0.0\", ['Elixir.Arc.Net.Relay.Acceptor']}}, point_of_no_return, {load, {'Elixir.Arc.Net.Relay.Acceptor', soft_purge, soft_purge}}]}], []}.\n"

    assert {:error, {:protected_module, Arc.Net.Relay.Acceptor}} =
             Package.validate_hot_relup(protected,
               target_version: "2.0.0",
               source_version: "1.0.0"
             )
  end

  test "rejects traversal and duplicate archive members before extraction", %{root: root} do
    file = Path.join(root, "payload")
    File.write!(file, "payload")
    traversal = archive_entries(root, "traversal", [{file, "../escape"}])

    assert {:error, {:unsafe_archive_path, "../escape"}} =
             Package.validate_archive(traversal,
               expected: expected(traversal, "2.0.0", "arc-build-2")
             )

    refute File.exists?(Path.join(root, "escape"))

    name = "lib/arc_net-2.0.0/ebin/arc_net.beam"
    duplicate = archive_entries(root, "duplicate", [{file, name}, {file, name}])

    assert {:error, {:duplicate_archive_path, ^name}} =
             Package.validate_archive(duplicate,
               expected: expected(duplicate, "2.0.0", "arc-build-2")
             )
  end

  test "rejects links and expansion beyond the configured limit", %{root: root} do
    file = Path.join(root, "payload")
    File.write!(file, String.duplicate("x", 1_024))
    link = Path.join(root, "link")
    File.ln_s!(file, link)
    name = "lib/arc_net-2.0.0/ebin/arc_net.beam"
    linked = archive_entries(root, "linked", [{link, name}])

    assert {:error, {:unsafe_archive_entry_type, ^name, :symlink}} =
             Package.validate_archive(linked, expected: expected(linked, "2.0.0", "arc-build-2"))

    large = archive_entries(root, "large", [{file, name}])

    assert {:error, {:archive_expands_too_large, 1_024, 100}} =
             Package.validate_archive(large,
               expected: expected(large, "2.0.0", "arc-build-2"),
               max_expanded_bytes: 100
             )
  end

  defp archive_entries(root, name, entries) do
    path = Path.join(root, name <> ".tar.gz")
    {:ok, tar} = :erl_tar.open(String.to_charlist(path), [:write, :compressed])

    try do
      Enum.each(entries, fn {source, destination} ->
        :ok = :erl_tar.add(tar, String.to_charlist(source), String.to_charlist(destination), [])
      end)
    after
      :ok = :erl_tar.close(tar)
    end

    path
  end

  defp build_archive(root, runtime \\ "16.3") do
    source = Path.join(root, "source")
    version = "2.0.0"
    release_dir = Path.join(source, "releases/#{version}")
    app_dir = Path.join(source, "lib/arc_net-#{version}/ebin")
    File.mkdir_p!(release_dir)
    File.mkdir_p!(app_dir)

    build = %{
      "version" => version,
      "build" => "arc-build-2",
      "runtime" => runtime,
      "platform" => %{"os" => "darwin", "arch" => "aarch64"}
    }

    rel =
      "{release, {\"arc_runtime\", \"2.0.0\"}, {erts, \"#{runtime}\"}, [{arc_net, \"2.0.0\", permanent}]}.\n"

    relup = valid_relup()

    files = %{
      "releases/arc_runtime.rel" => rel,
      "releases/#{version}/arc_runtime.rel" => rel,
      "releases/#{version}/start.boot" => <<131, 104, 0>>,
      "releases/#{version}/relup" => relup,
      "releases/#{version}/arc-build.json" => :json.encode(build),
      "lib/arc_net-#{version}/ebin/arc_net.app" => "{application, arc_net, []}.\n",
      "lib/arc_net-#{version}/ebin/arc_net.beam" => <<70, 79, 82, 49>>
    }

    Enum.each(files, fn {entry, contents} ->
      path = Path.join(source, entry)
      File.mkdir_p!(Path.dirname(path))
      File.write!(path, contents)
    end)

    archive = Path.join(root, "arc_runtime.tar.gz")
    {:ok, tar} = :erl_tar.open(String.to_charlist(archive), [:write, :compressed])

    try do
      Enum.each(Map.keys(files), fn entry ->
        :ok =
          :erl_tar.add(
            tar,
            String.to_charlist(Path.join(source, entry)),
            String.to_charlist(entry),
            [:compressed, :dereference]
          )
      end)
    after
      :ok = :erl_tar.close(tar)
    end

    archive
  end

  defp expected(archive, version, build) do
    %{
      version: version,
      build: build,
      runtime: "16.3",
      platform: %{os: "darwin", arch: "aarch64"},
      size: File.stat!(archive).size,
      sha256: :crypto.hash(:sha256, File.read!(archive)) |> Base.encode16(case: :lower)
    }
  end

  defp valid_relup do
    "{\"2.0.0\", [{\"1.0.0\", [], [{load_object_code, {arc_net, \"2.0.0\", ['Elixir.Arc.Net.Relay']}}, point_of_no_return, {suspend, ['Elixir.Arc.Net.Relay']}, {load, {'Elixir.Arc.Net.Relay', soft_purge, soft_purge}}, {code_change, up, [{'Elixir.Arc.Net.Relay', []}]}, {resume, ['Elixir.Arc.Net.Relay']}]}], [{\"1.0.0\", [], []}]}.\n"
  end
end
