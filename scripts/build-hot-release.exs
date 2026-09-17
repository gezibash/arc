#!/usr/bin/env elixir

# Produces an OTP release-handler package for one explicitly authored hot edge.
# This is deliberately separate from `mix release`: Mix does not author appup
# migrations or prove that a release can preserve a running service.

defmodule Arc.HotRelease.Builder do
  @release_name "arc_runtime"

  def run(argv) do
    with {:ok, options} <- parse(argv),
         {:ok, base} <- release(options.base),
         {:ok, candidate} <- release(options.candidate),
         :ok <- distinct_versions(base, candidate),
         :ok <- output_is_new(options.output),
         :ok <- validate_shared_libraries(base, candidate),
         :ok <- validate_bootstrap(base, candidate),
         :ok <- validate_start_script(candidate),
         :ok <- validate_boot_binary(candidate),
         :ok <- require_candidate_appups(base, candidate),
         {:ok, stage} <- stage_candidate(options.candidate),
         result <- build(stage, options, base, candidate) do
      File.rm_rf!(stage)
      result
    else
      {:error, reason} -> raise "hot release package was not built: #{format(reason)}"
    end
  end

  defp parse(argv) do
    {opts, rest, invalid} =
      OptionParser.parse(argv,
        strict: [
          base: :string,
          candidate: :string,
          build: :string,
          output: :string,
          os: :string,
          arch: :string
        ]
      )

    with true <- rest == [] and invalid == [],
         {:ok, base} <- required_path(opts, :base),
         {:ok, candidate} <- required_path(opts, :candidate),
         {:ok, build} <- required_string(opts, :build),
         {:ok, output} <- required_output(opts),
         {:ok, os} <- required_string(opts, :os),
         {:ok, arch} <- required_string(opts, :arch) do
      {:ok, %{base: base, candidate: candidate, build: build, output: output, os: os, arch: arch}}
    else
      _ -> {:error, :usage}
    end
  end

  defp required_path(opts, key) do
    with {:ok, path} <- required_string(opts, key),
         expanded = Path.expand(path),
         true <- File.dir?(expanded) do
      {:ok, expanded}
    else
      false -> {:error, {:not_a_directory, key}}
      error -> error
    end
  end

  defp required_output(opts) do
    with {:ok, output} <- required_string(opts, :output),
         output = Path.expand(output),
         true <- Path.extname(output) in [".gz", ".tgz"] do
      {:ok, output}
    else
      false -> {:error, :output_must_be_tar_gz}
      error -> error
    end
  end

  defp output_is_new(path) do
    if File.exists?(path), do: {:error, {:output_already_exists, path}}, else: :ok
  end

  defp required_string(opts, key) do
    case opts[key] do
      value when is_binary(value) and value != "" -> {:ok, value}
      _ -> {:error, {:missing_option, key}}
    end
  end

  defp release(root) do
    current = Path.join(root, "releases/start_erl.data")

    with {:ok, contents} <- File.read(current),
         [runtime, version] <- String.split(String.trim(contents), ~r/\s+/, parts: 2),
         {:ok, release} <-
           read_release(Path.join(root, "releases/#{version}/#{@release_name}.rel"), root),
         true <- release.runtime == runtime do
      {:ok, release}
    else
      false -> {:error, {:runtime_does_not_match_start_erl, root}}
      _ -> {:error, {:invalid_current_release, root}}
    end
  end

  defp read_release(path, root) do
    with {:ok, [{:release, {name, version}, {:erts, runtime}, applications}]} <-
           :file.consult(String.to_charlist(path)),
         true <-
           name == String.to_charlist(@release_name) and is_list(version) and is_list(runtime) and
             is_list(applications) do
      {:ok,
       %{
         root: root,
         release_dir: Path.dirname(path),
         version: List.to_string(version),
         runtime: List.to_string(runtime),
         applications: applications
       }}
    else
      _ -> {:error, {:invalid_release_file, path}}
    end
  end

  # `systools` packages every app in a .rel. It is only safe for the ARC
  # updater to include an app-version directory that is new to the installed
  # root. Version-equal directories must be byte-identical and omitted later.
  defp require_candidate_appups(base, candidate) do
    changed = changed_applications(base.applications, candidate.applications)

    Enum.reduce_while(changed, :ok, fn {app, version}, :ok ->
      appup = Path.join(candidate.root, "lib/#{app}-#{version}/ebin/#{app}.appup")

      if File.regular?(appup) do
        {:cont, :ok}
      else
        {:halt, {:error, {:missing_authored_appup, app, version}}}
      end
    end)
  end

  defp changed_applications(base, candidate) do
    base_versions = Map.new(base, fn spec -> {elem(spec, 0), elem(spec, 1)} end)

    candidate
    |> Enum.map(fn spec -> {elem(spec, 0), elem(spec, 1)} end)
    |> Enum.reject(fn {app, version} -> base_versions[app] == version end)
  end

  defp validate_shared_libraries(base, candidate) do
    base_versions = Map.new(base.applications, fn spec -> {elem(spec, 0), elem(spec, 1)} end)

    candidate.applications
    |> Enum.reduce_while(:ok, fn spec, :ok ->
      app = elem(spec, 0)
      version = elem(spec, 1)

      if base_versions[app] == version do
        case compare_trees(
               Path.join(base.root, "lib/#{app}-#{version}"),
               Path.join(candidate.root, "lib/#{app}-#{version}")
             ) do
          :ok -> {:cont, :ok}
          {:error, reason} -> {:halt, {:error, {:shared_library_changed, app, version, reason}}}
        end
      else
        {:cont, :ok}
      end
    end)
  end

  defp validate_bootstrap(base, candidate) do
    files = ["env.sh", "elixir", "iex", "remote.vm.args", "runtime.exs", "vm.args"]

    Enum.reduce_while(files, :ok, fn file, :ok ->
      case compare_files(
             Path.join(base.release_dir, file),
             Path.join(candidate.release_dir, file)
           ) do
        :ok -> {:cont, :ok}
        {:error, reason} -> {:halt, {:error, {:bootstrap_changed, file, reason}}}
      end
    end)
  end

  defp validate_start_script(candidate) do
    path = Path.join(candidate.release_dir, "start.script")

    case :file.consult(String.to_charlist(path)) do
      {:ok, [{:script, {name, version}, _instructions}]}
      when is_list(name) and is_list(version) ->
        if List.to_string(name) == @release_name and List.to_string(version) == candidate.version,
          do: :ok,
          else: {:error, {:candidate_start_script_mismatch, path}}

      _ ->
        {:error, {:invalid_candidate_start_script, path}}
    end
  end

  defp validate_boot_binary(candidate) do
    path = Path.join(candidate.release_dir, "start.boot")

    with {:ok, contents} <- File.read(path),
         {:script, {name, version}, _instructions} <- :erlang.binary_to_term(contents),
         true <-
           is_list(name) and is_list(version) and List.to_string(name) == @release_name and
             List.to_string(version) == candidate.version do
      :ok
    else
      _ -> {:error, {:candidate_start_boot_mismatch, path}}
    end
  rescue
    _ -> {:error, :invalid_candidate_start_boot}
  end

  defp compare_trees(left, right) do
    with {:ok, left_files} <- tree_files(left),
         {:ok, right_files} <- tree_files(right),
         true <- Map.keys(left_files) == Map.keys(right_files) do
      Enum.reduce_while(left_files, :ok, fn {relative, digest}, :ok ->
        if right_files[relative] == digest,
          do: {:cont, :ok},
          else: {:halt, {:error, {:content_mismatch, relative}}}
      end)
    else
      false -> {:error, :file_set_mismatch}
      {:error, _} = error -> error
    end
  end

  defp tree_files(root) do
    if File.dir?(root) do
      root
      |> Path.join("**/*")
      |> Path.wildcard(match_dot: true)
      |> Enum.reduce_while({:ok, %{}}, fn path, {:ok, files} ->
        case File.lstat(path) do
          {:ok, %{type: :directory}} ->
            {:cont, {:ok, files}}

          {:ok, %{type: :regular}} ->
            {:cont,
             {:ok,
              Map.put(
                files,
                Path.relative_to(path, root),
                :crypto.hash(:sha256, File.read!(path))
              )}}

          {:ok, _} ->
            {:halt, {:error, {:unsafe_file_type, Path.relative_to(path, root)}}}

          {:error, reason} ->
            {:halt, {:error, {:could_not_read, Path.relative_to(path, root), reason}}}
        end
      end)
    else
      {:error, :missing_directory}
    end
  end

  defp compare_files(left, right) do
    with {:ok, %{type: :regular}} <- File.lstat(left),
         {:ok, %{type: :regular}} <- File.lstat(right),
         {:ok, left_contents} <- File.read(left),
         {:ok, right_contents} <- File.read(right) do
      if left_contents == right_contents, do: :ok, else: {:error, :content_mismatch}
    else
      _ -> {:error, :missing_or_unsafe_file}
    end
  end

  defp stage_candidate(candidate_root) do
    stage =
      Path.join(
        System.tmp_dir!(),
        "arc-hot-release-" <> Base.encode16(:crypto.strong_rand_bytes(16), case: :lower)
      )

    target = Path.join(stage, "candidate")
    File.mkdir_p!(stage)

    case File.cp_r(candidate_root, target) do
      {:ok, _} ->
        {:ok, target}

      {:error, reason, _} ->
        File.rm_rf!(stage)
        {:error, {:could_not_stage_candidate, reason}}
    end
  end

  defp build(stage_root, options, base, candidate) do
    candidate = %{
      candidate
      | root: stage_root,
        release_dir:
          Path.join(stage_root, Path.relative_to(candidate.release_dir, options.candidate))
    }

    metadata_path = Path.join(candidate.release_dir, "arc-build.json")

    metadata = %{
      "version" => candidate.version,
      "build" => options.build,
      "runtime" => candidate.runtime,
      "platform" => %{"os" => options.os, "arch" => options.arch}
    }

    File.write!(metadata_path, :json.encode(metadata))
    relup_path = Path.join(candidate.release_dir, "relup")

    with :ok <- make_relup(base, candidate, relup_path),
         :ok <- make_tar(base, candidate, options.output),
         :ok <-
           omit_installed_libraries(
             options.output,
             base.root,
             metadata_path,
             candidate.release_dir,
             candidate.version
           ),
         :ok <- self_validate(options.output, metadata, base.version) do
      IO.puts(options.output)
      :ok
    end
  end

  defp make_relup(base, candidate, output) do
    base_name = "#{@release_name}-#{base.version}"
    base_rel = Path.join(candidate.release_dir, "#{base_name}.rel")
    File.cp!(Path.join(base.release_dir, "#{@release_name}.rel"), base_rel)

    paths = application_ebins(candidate.root) ++ application_ebins(base.root)

    result =
      File.cd!(candidate.release_dir, fn ->
        :systools.make_relup(
          String.to_charlist(@release_name),
          [String.to_charlist(base_name)],
          [String.to_charlist(base_name)],
          [
            {:path, Enum.map(paths, &String.to_charlist/1)},
            {:outdir, ~c"."},
            :warnings_as_errors,
            :silent
          ]
        )
      end)

    File.rm(base_rel)

    case result do
      {:ok, _relup, _module, []} ->
        if File.regular?(output), do: :ok, else: {:error, :relup_was_not_written}

      {:ok, _relup, _module, warnings} ->
        {:error, {:relup_warnings, warnings}}

      {:error, _module, reason} ->
        {:error, {:could_not_make_relup, reason}}

      other ->
        {:error, {:could_not_make_relup, other}}
    end
  end

  defp make_tar(base, candidate, output) do
    scratch =
      Path.join(
        System.tmp_dir!(),
        "arc-hot-release-tar-" <> Base.encode16(:crypto.strong_rand_bytes(16), case: :lower)
      )

    File.mkdir_p!(scratch)
    temporary_output = Path.join(scratch, "#{@release_name}.tar.gz")
    paths = application_ebins(candidate.root) ++ application_ebins(base.root)

    try do
      result =
        File.cd!(candidate.release_dir, fn ->
          :systools.make_tar(
            String.to_charlist(@release_name),
            [
              {:path, Enum.map(paths, &String.to_charlist/1)},
              {:outdir, String.to_charlist(scratch)},
              :warnings_as_errors,
              :silent
            ]
          )
        end)

      case result do
        {:ok, _module, []} ->
          if File.exists?(output),
            do: {:error, {:output_already_exists, output}},
            else: File.rename(temporary_output, output)

        {:ok, _module, warnings} ->
          {:error, {:tar_warnings, warnings}}

        {:error, _module, reason} ->
          {:error, {:could_not_make_tar, reason}}

        other ->
          {:error, {:could_not_make_tar, other}}
      end
    after
      File.rm_rf!(scratch)
    end
  end

  # `systools:make_tar/2` includes every app named by the release. Rebuild its
  # archive without app-version directories that are already present under the
  # base release. Those applications remain available to release_handler via
  # the persistent root and must not be overwritten.
  defp omit_installed_libraries(archive, base_root, metadata_path, release_dir, version) do
    unpacked =
      Path.join(
        System.tmp_dir!(),
        "arc-hot-release-unpack-" <> Base.encode16(:crypto.strong_rand_bytes(16), case: :lower)
      )

    filtered = archive <> ".filtered"
    File.mkdir_p!(unpacked)

    try do
      with :ok <-
             :erl_tar.extract(String.to_charlist(archive), [
               :compressed,
               {:cwd, String.to_charlist(unpacked)}
             ]),
           {:ok, entries} <- :erl_tar.table(String.to_charlist(archive), [:compressed]),
           :ok <-
             write_filtered_archive(
               filtered,
               unpacked,
               entries,
               base_root,
               metadata_path,
               release_dir,
               version
             ) do
        File.rename!(filtered, archive)
        :ok
      else
        {:error, reason} -> {:error, {:could_not_filter_tar, reason}}
        error -> {:error, {:could_not_filter_tar, error}}
      end
    after
      File.rm_rf!(unpacked)
      File.rm(filtered)
    end
  end

  defp write_filtered_archive(
         path,
         unpacked,
         entries,
         base_root,
         metadata_path,
         release_dir,
         version
       ) do
    {:ok, tar} = :erl_tar.open(String.to_charlist(path), [:write, :compressed])

    try do
      Enum.reduce_while(entries, :ok, fn entry, :ok ->
        name = List.to_string(entry)

        if installed_library?(name, base_root) do
          {:cont, :ok}
        else
          case :erl_tar.add(
                 tar,
                 String.to_charlist(Path.join(unpacked, name)),
                 String.to_charlist(name),
                 [:compressed, :dereference]
               ) do
            :ok -> {:cont, :ok}
            {:error, reason} -> {:halt, {:error, reason}}
          end
        end
      end)
      |> case do
        :ok ->
          with :ok <-
                 :erl_tar.add(
                   tar,
                   String.to_charlist(metadata_path),
                   String.to_charlist("releases/#{version}/arc-build.json"),
                   [:compressed, :dereference]
                 ) do
            add_bootstrap_files(tar, release_dir, version)
          end

        error ->
          error
      end
    after
      :ok = :erl_tar.close(tar)
    end
  end

  defp add_bootstrap_files(tar, release_dir, version) do
    files =
      [
        "env.sh",
        "elixir",
        "iex",
        "remote.vm.args",
        "runtime.exs",
        "start_clean.boot",
        "start_clean.script",
        "start.script",
        "vm.args"
      ] ++
        (release_dir
         |> Path.join("consolidated/*.beam")
         |> Path.wildcard()
         |> Enum.map(&Path.relative_to(&1, release_dir)))

    Enum.reduce_while(files, :ok, fn relative, :ok ->
      source = Path.join(release_dir, relative)

      if File.regular?(source) do
        case :erl_tar.add(
               tar,
               String.to_charlist(source),
               String.to_charlist("releases/#{version}/#{relative}"),
               [:compressed, :dereference]
             ) do
          :ok -> {:cont, :ok}
          {:error, reason} -> {:halt, {:error, reason}}
        end
      else
        {:halt, {:error, {:missing_bootstrap_file, relative}}}
      end
    end)
  end

  defp installed_library?("lib/" <> rest, base_root) do
    [app_version | _] = String.split(rest, "/", parts: 2)
    File.exists?(Path.join([base_root, "lib", app_version]))
  end

  defp installed_library?(_name, _base_root), do: false

  defp self_validate(archive, metadata, source_version) do
    expected = Map.put(metadata, "size", File.stat!(archive).size)

    expected =
      Map.put(
        expected,
        "sha256",
        :crypto.hash(:sha256, File.read!(archive)) |> Base.encode16(case: :lower)
      )

    case Arc.CLI.Update.Package.validate_archive(archive,
           expected: expected,
           source_version: source_version
         ) do
      {:ok, _} -> :ok
      {:error, reason} -> {:error, {:package_preflight_failed, reason}}
    end
  end

  defp application_ebins(root), do: Path.wildcard(Path.join(root, "lib/*/ebin"))

  defp distinct_versions(base, candidate) when base.version != candidate.version, do: :ok
  defp distinct_versions(_base, _candidate), do: {:error, :base_and_candidate_versions_match}

  defp format(:usage) do
    "usage: mix run scripts/build-hot-release.exs -- --base ROOT --candidate ROOT --build IMMUTABLE_BUILD --os OS --arch ARCH --output PACKAGE.tar.gz"
  end

  defp format(reason), do: inspect(reason)
end

Arc.HotRelease.Builder.run(System.argv())
