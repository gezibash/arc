defmodule Arc.CLI.Update.Package do
  @moduledoc false

  @default_max_bytes 512 * 1_024 * 1_024
  @default_max_expanded_bytes 2 * 1_024 * 1_024 * 1_024
  @default_max_entries 50_000
  @max_build_metadata_bytes 64 * 1_024
  @max_relup_bytes 2 * 1_024 * 1_024
  @release_name "arc_runtime"

  @type expected :: %{
          required(:version) => String.t(),
          required(:build) => String.t(),
          required(:runtime) => String.t(),
          required(:platform) => %{required(:os) => String.t(), required(:arch) => String.t()},
          required(:size) => non_neg_integer(),
          required(:sha256) => String.t()
        }

  @doc """
  Validates an OTP full-release archive before it reaches `release_handler`.

  The digest and byte size are checked before the archive is inspected. Archive
  members are accepted only from the OTP full-release layout used by ARC's
  hot-only updater. This function never extracts an archive to its destination.
  """
  @spec validate_archive(Path.t(), keyword()) :: {:ok, map()} | {:error, term()}
  def validate_archive(path, opts) when is_binary(path) and is_list(opts) do
    with {:ok, expected} <- expected(opts),
         {:ok, size} <- file_size(path),
         :ok <-
           verify_size(size, expected.size, Keyword.get(opts, :max_bytes, @default_max_bytes)),
         :ok <- verify_digest(path, expected.sha256),
         {:ok, entries} <- table(path),
         :ok <-
           verify_archive_entries(
             entries,
             expected,
             Keyword.get(opts, :max_entries, @default_max_entries),
             Keyword.get(opts, :installed_root),
             Keyword.get(opts, :max_expanded_bytes, @default_max_expanded_bytes)
           ),
         {:ok, release_resource} <- archive_file(path, "releases/#{@release_name}.rel"),
         {:ok, versioned_release_resource} <-
           archive_file(path, "releases/#{expected.version}/#{@release_name}.rel"),
         :ok <-
           verify_release_resources(
             release_resource,
             versioned_release_resource,
             expected
           ),
         {:ok, build_json} <- archive_file(path, build_path(expected.version)),
         {:ok, metadata} <- decode_build_metadata(build_json, expected),
         {:ok, relup} <- archive_file(path, relup_path(expected.version)),
         {:ok, relup_info} <-
           validate_hot_relup(relup,
             target_version: expected.version,
             source_version: Keyword.get(opts, :source_version),
             allowed_migrations: Keyword.get(opts, :allowed_migrations, []),
             allowed_modules: Keyword.get(opts, :allowed_modules, [Arc.Net.Relay])
           ) do
      {:ok,
       %{
         version: expected.version,
         build: expected.build,
         runtime: expected.runtime,
         platform: expected.platform,
         size: size,
         sha256: expected.sha256,
         relup_sha256: sha256(relup),
         entries: Enum.map(entries, &entry_name/1),
         metadata: metadata,
         relup: relup_info
       }}
    end
  end

  @doc """
  Validates the low-level relup script that `release_handler` would execute.

  `:apply` instructions are refused unless their exact `{module, function,
  arguments}` tuple appears in `:allowed_migrations`. The normal advanced
  `code_change` path does not need an `apply` entry.
  """
  @spec validate_hot_relup(binary(), keyword()) :: {:ok, map()} | {:error, term()}
  def validate_hot_relup(relup, opts \\ []) when is_binary(relup) and is_list(opts) do
    max_bytes = Keyword.get(opts, :max_bytes, @max_relup_bytes)

    with :ok <- verify_relup_size(relup, max_bytes),
         {:ok, {target, upgrades, downgrades}} <- consult_term(relup),
         :ok <- verify_target(target, Keyword.get(opts, :target_version)),
         :ok <- verify_source(upgrades, Keyword.get(opts, :source_version)),
         :ok <-
           verify_scripts(
             upgrades,
             Keyword.get(opts, :allowed_migrations, []),
             Keyword.get(opts, :allowed_modules, [Arc.Net.Relay])
           ),
         :ok <-
           verify_scripts(
             downgrades,
             Keyword.get(opts, :allowed_migrations, []),
             Keyword.get(opts, :allowed_modules, [Arc.Net.Relay])
           ) do
      {:ok,
       %{
         target_version: List.to_string(target),
         upgrade_sources: Enum.map(upgrades, &script_source/1),
         downgrade_targets: Enum.map(downgrades, &script_source/1),
         sha256: sha256(relup)
       }}
    end
  end

  defp expected(opts) do
    case Keyword.fetch(opts, :expected) do
      {:ok, expected} when is_map(expected) -> normalize_expected(expected)
      :error -> {:error, :missing_expected_release}
      _ -> {:error, :invalid_expected_release}
    end
  end

  defp normalize_expected(expected) do
    with {:ok, version} <- required_string(expected, :version),
         {:ok, build} <- required_string(expected, :build),
         {:ok, runtime} <- required_string(expected, :runtime),
         {:ok, platform} <-
           normalize_platform(Map.get(expected, :platform) || Map.get(expected, "platform")),
         {:ok, size} <- required_non_negative_integer(expected, :size),
         {:ok, digest} <- required_digest(expected, :sha256) do
      {:ok,
       %{
         version: version,
         build: build,
         runtime: runtime,
         platform: platform,
         size: size,
         sha256: digest
       }}
    end
  end

  defp normalize_platform(platform) when is_map(platform) do
    with {:ok, os} <- required_string(platform, :os),
         {:ok, arch} <- required_string(platform, :arch) do
      {:ok, %{os: os, arch: arch}}
    end
  end

  defp normalize_platform(_), do: {:error, :invalid_platform}

  defp required_string(map, key) do
    value = Map.get(map, key) || Map.get(map, Atom.to_string(key))

    if is_binary(value) and value != "",
      do: {:ok, value},
      else: {:error, {:missing_or_invalid, key}}
  end

  defp required_non_negative_integer(map, key) do
    value = Map.get(map, key) || Map.get(map, Atom.to_string(key))

    if is_integer(value) and value >= 0,
      do: {:ok, value},
      else: {:error, {:missing_or_invalid, key}}
  end

  defp required_digest(map, key) do
    with {:ok, digest} <- required_string(map, key),
         true <- digest =~ ~r/\A[0-9a-fA-F]{64}\z/ do
      {:ok, String.downcase(digest)}
    else
      false -> {:error, {:missing_or_invalid, key}}
      error -> error
    end
  end

  defp file_size(path) do
    case File.stat(path) do
      {:ok, %{type: :regular, size: size}} -> {:ok, size}
      {:ok, _} -> {:error, :archive_is_not_regular_file}
      {:error, reason} -> {:error, {:archive_unavailable, reason}}
    end
  end

  defp verify_size(size, expected, max) when size == expected and size <= max, do: :ok

  defp verify_size(_size, _expected, max) when not is_integer(max) or max <= 0,
    do: {:error, :invalid_max_bytes}

  defp verify_size(size, _expected, max) when size > max,
    do: {:error, {:archive_too_large, size, max}}

  defp verify_size(size, expected, _max), do: {:error, {:archive_size_mismatch, expected, size}}

  defp verify_digest(path, expected) do
    case File.open(path, [:read, :binary]) do
      {:ok, device} ->
        try do
          actual =
            digest_device(device, :crypto.hash_init(:sha256)) |> Base.encode16(case: :lower)

          if actual == expected, do: :ok, else: {:error, :archive_digest_mismatch}
        after
          File.close(device)
        end

      {:error, reason} ->
        {:error, {:archive_unavailable, reason}}
    end
  end

  defp digest_device(device, state) do
    case IO.binread(device, 1_048_576) do
      :eof -> :crypto.hash_final(state)
      {:error, reason} -> throw({:archive_read_failed, reason})
      data -> digest_device(device, :crypto.hash_update(state, data))
    end
  end

  defp table(path) do
    case :erl_tar.table(String.to_charlist(path), [:compressed, :verbose]) do
      {:ok, entries} -> {:ok, entries}
      {:error, reason} -> {:error, {:invalid_archive, reason}}
    end
  end

  defp verify_archive_entries(entries, expected, max_entries, installed_root, max_expanded_bytes)
       when is_integer(max_entries) and max_entries > 0 and is_integer(max_expanded_bytes) and
              max_expanded_bytes > 0 do
    cond do
      length(entries) > max_entries ->
        {:error, {:too_many_archive_entries, length(entries), max_entries}}

      expanded_size(entries) > max_expanded_bytes ->
        {:error, {:archive_expands_too_large, expanded_size(entries), max_expanded_bytes}}

      true ->
        verify_entry_list(entries, expected, %{}, installed_root)
    end
  end

  defp verify_archive_entries(
         _entries,
         _expected,
         _max_entries,
         _installed_root,
         _max_expanded_bytes
       ),
       do: {:error, :invalid_archive_limits}

  defp verify_entry_list([], expected, seen, _installed_root) do
    required = required_entries(expected.version)
    missing = Enum.reject(required, &Map.has_key?(seen, &1))
    if missing == [], do: :ok, else: {:error, {:missing_archive_entries, missing}}
  end

  defp verify_entry_list([entry | rest], expected, seen, installed_root) do
    name = entry_name(entry)

    cond do
      not safe_path?(name) ->
        {:error, {:unsafe_archive_path, name}}

      Map.has_key?(seen, name) ->
        {:error, {:duplicate_archive_path, name}}

      not regular_or_directory?(entry) ->
        {:error, {:unsafe_archive_entry_type, name, entry_type(entry)}}

      not allowed_path?(name, entry_type(entry), expected.version) ->
        {:error, {:unexpected_archive_path, name}}

      not bounded_named_entry?(name, entry_size(entry)) ->
        {:error, {:oversized_archive_entry, name, entry_size(entry)}}

      existing_library_destination?(name, installed_root) ->
        {:error, {:existing_library_destination, name}}

      true ->
        verify_entry_list(rest, expected, Map.put(seen, name, true), installed_root)
    end
  end

  defp entry_name({name, _type, _size, _mtime, _mode, _uid, _gid}) when is_list(name),
    do: List.to_string(name)

  defp entry_type({_name, type, _size, _mtime, _mode, _uid, _gid}), do: type
  defp entry_size({_name, _type, size, _mtime, _mode, _uid, _gid}), do: size
  defp regular_or_directory?(entry), do: entry_type(entry) in [:regular, :directory]
  defp expanded_size(entries), do: Enum.sum(Enum.map(entries, &entry_size/1))

  defp bounded_named_entry?(name, size) do
    cond do
      String.ends_with?(name, "/arc-build.json") -> size <= @max_build_metadata_bytes
      String.ends_with?(name, "/relup") -> size <= @max_relup_bytes
      String.ends_with?(name, ".rel") -> size <= @max_build_metadata_bytes
      true -> true
    end
  end

  defp safe_path?(name) do
    Path.type(name) not in [:absolute, :volumerelative] and
      name != "" and
      String.valid?(name) and
      String.split(name, "/", trim: false) |> Enum.all?(&(&1 not in ["", ".", ".."]))
  end

  defp required_entries(version) do
    [
      "releases/#{@release_name}.rel",
      "releases/#{version}/#{@release_name}.rel",
      "releases/#{version}/start.boot",
      relup_path(version),
      build_path(version)
    ]
  end

  defp build_path(version), do: "releases/#{version}/arc-build.json"
  defp relup_path(version), do: "releases/#{version}/relup"

  defp allowed_path?(name, :directory, version) do
    name in [
      "releases",
      "releases/#{version}",
      "lib"
    ] or
      Regex.match?(~r/\Alib\/[^\/]+-[^\/]+(?:\/(?:ebin|priv))?\z/, name)
  end

  defp allowed_path?(name, :regular, version) do
    name in [
      "releases/#{@release_name}.rel",
      "releases/#{version}/#{@release_name}.rel",
      "releases/#{version}/start.boot",
      "releases/#{version}/relup",
      "releases/#{version}/sys.config",
      "releases/#{version}/sys.config.src",
      "releases/#{version}/arc-build.json",
      "releases/#{version}/env.sh",
      "releases/#{version}/elixir",
      "releases/#{version}/iex",
      "releases/#{version}/remote.vm.args",
      "releases/#{version}/runtime.exs",
      "releases/#{version}/start_clean.boot",
      "releases/#{version}/start_clean.script",
      "releases/#{version}/start.script",
      "releases/#{version}/vm.args"
    ] or
      Regex.match?(~r/\Alib\/[^\/]+-[^\/]+\/ebin\/[^\/]+\.(?:beam|app|appup)\z/, name) or
      Regex.match?(~r/\Alib\/[^\/]+-[^\/]+\/priv\/.+\z/, name) or
      Regex.match?(~r/\Areleases\/[^\/]+\/consolidated\/[^\/]+\.beam\z/, name)
  end

  defp allowed_path?(_name, _type, _version), do: false

  # A full release's `.rel` may refer to an already-installed dependency. The
  # archive must omit that existing application directory; `release_handler`
  # can use it from the persistent release root. Extracting another copy risks
  # overwriting bytes that the running node can still load.
  defp existing_library_destination?("lib/" <> rest, installed_root)
       when is_binary(installed_root) do
    [app_version | _] = String.split(rest, "/", parts: 2)
    File.exists?(Path.join([installed_root, "lib", app_version]))
  end

  defp existing_library_destination?(_name, _installed_root), do: false

  defp archive_file(path, name) do
    case :erl_tar.extract(String.to_charlist(path), [
           :compressed,
           :memory,
           {:files, [String.to_charlist(name)]}
         ]) do
      {:ok, [{_name, contents}]} when is_binary(contents) -> {:ok, contents}
      {:ok, _} -> {:error, {:missing_archive_entry, name}}
      {:error, reason} -> {:error, {:could_not_read_archive_entry, name, reason}}
    end
  end

  defp decode_build_metadata(build_json, expected) do
    metadata = :json.decode(build_json)

    if is_map(metadata) do
      case compare_metadata(metadata, expected) do
        :ok -> {:ok, metadata}
        {:error, _} = error -> error
      end
    else
      {:error, :invalid_build_metadata}
    end
  rescue
    _ -> {:error, :invalid_build_metadata}
  end

  defp verify_release_resources(release_resource, versioned_release_resource, expected) do
    with true <- release_resource == versioned_release_resource,
         {:ok, {:release, {name, version}, {:erts, runtime}, _applications}} <-
           consult_one_term(release_resource),
         true <-
           release_name?(name) and is_list(version) and is_list(runtime) and
             List.to_string(version) == expected.version and
             List.to_string(runtime) == expected.runtime do
      :ok
    else
      false -> {:error, :release_resource_mismatch}
      _ -> {:error, :invalid_release_resource}
    end
  end

  # Mix emits a charlist release name. Keep the historical atom form readable
  # for pre-channel artifacts, but publishers emit only the canonical charlist.
  defp release_name?(:arc_runtime), do: true
  defp release_name?(name) when is_list(name), do: List.to_string(name) == @release_name
  defp release_name?(_name), do: false

  defp compare_metadata(metadata, expected) do
    platform = metadata["platform"]

    if metadata["version"] == expected.version and metadata["build"] == expected.build and
         metadata["runtime"] == expected.runtime and is_map(platform) and
         platform["os"] == expected.platform.os and platform["arch"] == expected.platform.arch do
      :ok
    else
      {:error, :build_metadata_mismatch}
    end
  end

  defp verify_relup_size(relup, max) when byte_size(relup) <= max and max > 0, do: :ok

  defp verify_relup_size(_relup, max) when not is_integer(max) or max <= 0,
    do: {:error, :invalid_relup_max_bytes}

  defp verify_relup_size(relup, max), do: {:error, {:relup_too_large, byte_size(relup), max}}

  defp consult_term(contents) do
    with {:ok, term} <- consult_one_term(contents) do
      case term do
        {target, upgrades, downgrades}
        when is_list(target) and is_list(upgrades) and is_list(downgrades) ->
          {:ok, {target, upgrades, downgrades}}

        _ ->
          {:error, :invalid_relup}
      end
    end
  end

  defp consult_one_term(contents) do
    path =
      Path.join(
        System.tmp_dir!(),
        "arc-relup-" <> Base.encode16(:crypto.strong_rand_bytes(16), case: :lower)
      )

    try do
      :ok = File.write(path, contents)

      case :file.consult(String.to_charlist(path)) do
        {:ok, [term]} -> {:ok, term}
        _ -> {:error, :invalid_term}
      end
    after
      File.rm(path)
    end
  end

  defp verify_target(_target, nil), do: :ok

  defp verify_target(target, expected) when is_binary(expected) do
    if List.to_string(target) == expected,
      do: :ok,
      else: {:error, {:relup_target_mismatch, expected}}
  end

  defp verify_target(_target, _expected), do: {:error, :invalid_target_version}

  defp verify_source(_scripts, nil), do: :ok

  defp verify_source(scripts, expected) when is_binary(expected) do
    if Enum.any?(scripts, &(script_source(&1) == expected)),
      do: :ok,
      else: {:error, {:relup_source_missing, expected}}
  end

  defp verify_source(_scripts, _expected), do: {:error, :invalid_source_version}

  defp verify_scripts(scripts, migrations, modules)
       when is_list(migrations) and is_list(modules) do
    Enum.reduce_while(scripts, :ok, fn script, :ok ->
      with {:ok, _source, instructions} <- script_instructions(script),
           :ok <- verify_instructions(instructions, migrations, modules) do
        {:cont, :ok}
      else
        {:error, _} = error -> {:halt, error}
      end
    end)
  end

  defp verify_scripts(_scripts, _migrations, _modules), do: {:error, :invalid_hot_allowlist}

  defp script_source({source, _description, _instructions}) when is_list(source),
    do: List.to_string(source)

  defp script_source(_), do: nil

  defp script_instructions({source, _description, instructions})
       when is_list(source) and is_list(instructions),
       do: {:ok, List.to_string(source), instructions}

  defp script_instructions(_), do: {:error, :invalid_relup_script}

  defp verify_instructions(instructions, migrations, modules) do
    Enum.reduce_while(instructions, :ok, fn instruction, :ok ->
      case verify_instruction(instruction, migrations, modules) do
        :ok -> {:cont, :ok}
        {:error, _} = error -> {:halt, error}
      end
    end)
  end

  defp verify_instruction(instruction, _migrations, _modules)
       when instruction in [:point_of_no_return], do: :ok

  defp verify_instruction(
         {:load_object_code, {_app, _version, changed_modules}},
         _migrations,
         modules
       )
       when is_list(changed_modules),
       do: allow_modules(changed_modules, modules)

  defp verify_instruction({:load, {module, :soft_purge, :soft_purge}}, _migrations, modules) do
    allow_module(module, modules)
  end

  defp verify_instruction({:remove, {module, :soft_purge, :soft_purge}}, _migrations, modules) do
    allow_module(module, modules)
  end

  defp verify_instruction({:suspend, changed_modules}, _migrations, modules)
       when is_list(changed_modules),
       do: allow_modules(suspended_modules(changed_modules), modules)

  defp verify_instruction({:resume, changed_modules}, _migrations, modules)
       when is_list(changed_modules),
       do: allow_modules(changed_modules, modules)

  defp verify_instruction({:code_change, direction, changed_modules}, _migrations, modules)
       when direction in [:up, :down] and is_list(changed_modules),
       do: allow_code_change_modules(changed_modules, modules)

  defp verify_instruction({:code_change, changed_modules}, _migrations, modules)
       when is_list(changed_modules),
       do: allow_code_change_modules(changed_modules, modules)

  defp verify_instruction({:purge, _purge_modules}, _migrations, _allowed_modules),
    do: {:error, {:restart_or_hard_purge_instruction, :purge}}

  defp verify_instruction({instruction, _rest}, _migrations, _modules)
       when instruction in [:stop, :start, :sync_nodes],
       do: {:error, {:unsupported_hot_instruction, instruction}}

  defp verify_instruction({instruction, _one, _two}, _migrations, _modules)
       when instruction in [:stop, :start, :sync_nodes],
       do: {:error, {:unsupported_hot_instruction, instruction}}

  defp verify_instruction({instruction, _one, _two, _three}, _migrations, _modules)
       when instruction in [:stop, :start, :sync_nodes],
       do: {:error, {:unsupported_hot_instruction, instruction}}

  defp verify_instruction(
         {:apply, {module, function, arguments}} = instruction,
         migrations,
         _modules
       )
       when is_atom(module) and is_atom(function) and is_list(arguments) do
    if {module, function, arguments} in migrations,
      do: :ok,
      else: {:error, {:unapproved_migration, instruction}}
  end

  defp verify_instruction(
         {:apply, module, function, arguments} = instruction,
         migrations,
         _modules
       )
       when is_atom(module) and is_atom(function) and is_list(arguments) do
    if {module, function, arguments} in migrations,
      do: :ok,
      else: {:error, {:unapproved_migration, instruction}}
  end

  defp verify_instruction(instruction, _migrations, _modules) do
    case forbidden_instruction(instruction) do
      nil -> {:error, {:unsupported_hot_instruction, instruction}}
      forbidden -> {:error, {:restart_or_hard_purge_instruction, forbidden}}
    end
  end

  defp allow_module(module, modules) do
    if module in modules, do: :ok, else: {:error, {:protected_module, module}}
  end

  defp allow_modules(changed_modules, modules) do
    case Enum.find(changed_modules, &(allow_module(&1, modules) != :ok)) do
      nil -> :ok
      module -> {:error, {:protected_module, module}}
    end
  end

  defp allow_code_change_modules(changed_modules, modules) do
    changed_modules
    |> Enum.map(fn
      {module, _extra} -> module
      module -> module
    end)
    |> allow_modules(modules)
  end

  defp suspended_modules(changed_modules) do
    Enum.map(changed_modules, fn
      {module, _timeout} -> module
      module -> module
    end)
  end

  defp forbidden_instruction(term)
       when is_atom(term) and
              term in [
                :restart_new_emulator,
                :restart_emulator,
                :restart,
                :reboot,
                :brutal_purge,
                :purge
              ],
       do: term

  defp forbidden_instruction(term) when is_tuple(term) do
    term |> Tuple.to_list() |> Enum.find_value(&forbidden_instruction/1)
  end

  defp forbidden_instruction(term) when is_list(term),
    do: Enum.find_value(term, &forbidden_instruction/1)

  defp forbidden_instruction(_term), do: nil

  defp sha256(binary), do: :crypto.hash(:sha256, binary) |> Base.encode16(case: :lower)
end
