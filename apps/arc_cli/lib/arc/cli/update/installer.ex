defmodule Arc.CLI.Update.Installer do
  @moduledoc """
  Replaces one complete ARC installation with a verified release archive.

  The archive is the complete release tarball the install script unpacks:
  every member lives under `arc/`. Validation checks the exact byte length and
  SHA-256 digest, refuses unsafe paths and non-regular members, and requires
  the release layout for the expected version.

  Installation extracts beside the current root, starts the new `bin/arc` to
  prove it boots and reports the expected version, and only then swaps the
  directories with two renames. The replaced release stays beside the root as
  `<root>.previous` until the next successful update removes it. The running
  process keeps its already loaded code; nothing is hot-loaded.
  """

  @prefix "arc/"
  @max_entries 50_000
  @max_expanded_bytes 2 * 1_024 * 1_024 * 1_024
  @max_version_file_bytes 256
  @probe_timeout_ms 60_000
  @required ["arc/bin/arc", "arc/bin/arc_runtime", "arc/releases/start_erl.data"]
  @version_pattern ~r/\A[0-9A-Za-z.+_-]{1,128}\z/
  @hex_pattern ~r/\A[0-9a-f]{64}\z/

  @doc """
  Validates a complete release archive before anything is extracted.

  Options are `:version`, `:sha256` (lower case hexadecimal), and `:size`.
  """
  @spec validate_archive(Path.t(), keyword()) :: {:ok, map()} | {:error, term()}
  def validate_archive(path, opts) when is_binary(path) and is_list(opts) do
    with {:ok, version} <- expected_version(opts),
         {:ok, digest} <- expected_digest(opts),
         {:ok, expected_size} <- expected_size(opts),
         {:ok, size} <- file_size(path),
         true <- size == expected_size or {:error, :archive_size_mismatch},
         :ok <- verify_digest(path, digest),
         {:ok, entries} <- table(path),
         :ok <- verify_entries(entries, version) do
      {:ok, %{version: version, size: size, sha256: digest, entries: length(entries)}}
    end
  end

  @doc """
  Installs a validated archive over `root`.

  The archive members are checked again before extraction. `:version` is the
  release version the new installation must report. `:probe` overrides the
  boot check for tests; it receives the candidate root and returns the output
  of `bin/arc version`.
  """
  @spec install(Path.t(), Path.t(), keyword()) :: {:ok, map()} | {:error, term()}
  def install(archive, root, opts)
      when is_binary(archive) and is_binary(root) and is_list(opts) do
    with {:ok, version} <- expected_version(opts),
         :ok <- verify_root(root),
         {:ok, entries} <- table(archive),
         :ok <- verify_entries(entries, version),
         {:ok, staging} <- open_staging(root) do
      result = install_from_staging(archive, root, staging, version, opts)
      cleanup_staging(staging, root)
      result
    end
  end

  @doc "Checks that `root` is an absolute, real directory holding an ARC release."
  @spec verify_root(Path.t()) :: :ok | {:error, term()}
  def verify_root(root) when is_binary(root) do
    cond do
      Path.type(root) != :absolute ->
        {:error, :install_root_not_absolute}

      not match?({:ok, %{type: :directory}}, File.lstat(root)) ->
        {:error, :install_root_invalid}

      not File.regular?(Path.join([root, "bin", "arc"])) ->
        {:error, :install_root_invalid}

      not File.regular?(Path.join([root, "releases", "start_erl.data"])) ->
        {:error, :install_root_invalid}

      true ->
        :ok
    end
  end

  def verify_root(_), do: {:error, :install_root_invalid}

  @doc "Reads the release version an installation boots, from `releases/start_erl.data`."
  @spec installed_version(Path.t()) :: {:ok, String.t()} | {:error, term()}
  def installed_version(root) when is_binary(root) do
    path = Path.join([root, "releases", "start_erl.data"])

    with {:ok, %{type: :regular, size: size}} when size <= @max_version_file_bytes <-
           File.lstat(path),
         {:ok, bytes} <- File.read(path),
         [_erts, version] <- String.split(bytes, ~r/\s+/, trim: true),
         true <- Regex.match?(@version_pattern, version) do
      {:ok, version}
    else
      _ -> {:error, :installed_version_unreadable}
    end
  end

  defp install_from_staging(archive, root, staging, version, opts) do
    candidate = Path.join(staging, "arc")
    previous = root <> ".previous"

    with :ok <- extract(archive, staging),
         :ok <- verify_candidate(candidate, version),
         :ok <- probe(candidate, version, opts),
         :ok <- remove_stale_previous(previous),
         :ok <- swap(root, previous, candidate) do
      {:ok, %{root: root, previous: previous, version: version}}
    end
  end

  # After a swap the staging directory is empty. After a failure it holds the
  # rejected candidate. If the root itself is missing, the candidate is the
  # only complete release nearby, so it is deliberately left for recovery.
  defp cleanup_staging(staging, root) do
    if File.dir?(root), do: File.rm_rf(staging)
    :ok
  end

  defp open_staging(root) do
    suffix = Base.url_encode64(:crypto.strong_rand_bytes(9), padding: false)
    path = Path.join(Path.dirname(root), ".#{Path.basename(root)}.update-#{suffix}")

    with :ok <- File.mkdir(path),
         :ok <- File.chmod(path, 0o700) do
      {:ok, path}
    else
      _ -> {:error, :staging_directory_failed}
    end
  end

  defp extract(archive, staging) do
    case :erl_tar.extract(String.to_charlist(archive), [
           :compressed,
           {:cwd, String.to_charlist(staging)}
         ]) do
      :ok -> :ok
      {:error, reason} -> {:error, {:extract_failed, reason}}
    end
  end

  defp verify_candidate(candidate, version) do
    with true <-
           File.regular?(Path.join([candidate, "bin", "arc"])) or {:error, :candidate_invalid},
         {:ok, ^version} <- installed_version(candidate) do
      :ok
    else
      {:ok, _other} -> {:error, :candidate_version_mismatch}
      {:error, _} = error -> error
    end
  end

  defp probe(candidate, version, opts) do
    probe = Keyword.get(opts, :probe, &run_version/1)

    case probe.(candidate) do
      {:ok, output} when is_binary(output) ->
        if version_output?(output, version),
          do: :ok,
          else: {:error, {:candidate_reports_other_version, first_line(output)}}

      {:error, reason} ->
        {:error, {:candidate_start_failed, reason}}
    end
  end

  defp version_output?(output, version) do
    case String.split(String.trim(output), ~r/\s+/, parts: 3) do
      ["arc", ^version | _] -> true
      _ -> false
    end
  end

  defp first_line(output) do
    output |> String.split("\n", parts: 2) |> hd() |> String.slice(0, 200)
  end

  # The candidate is started in a fresh environment. The running release
  # exports RELEASE_* variables that would otherwise point the candidate's
  # control script at this installation's version directory.
  defp run_version(candidate) do
    bin = Path.join([candidate, "bin", "arc"])

    env =
      System.get_env()
      |> Map.keys()
      |> Enum.filter(&String.starts_with?(&1, "RELEASE_"))
      |> Enum.map(&{&1, nil})

    task =
      Task.async(fn ->
        try do
          System.cmd(bin, ["version"], stderr_to_stdout: true, env: env)
        rescue
          error -> {:error, Exception.message(error)}
        end
      end)

    case Task.yield(task, @probe_timeout_ms) || Task.shutdown(task, :brutal_kill) do
      {:ok, {output, 0}} -> {:ok, output}
      {:ok, {_output, status}} when is_integer(status) -> {:error, {:exit_status, status}}
      {:ok, {:error, reason}} -> {:error, reason}
      {:exit, _reason} -> {:error, :probe_crashed}
      nil -> {:error, :probe_timeout}
    end
  end

  # Only a directory this installer created earlier is removed: it must hold a
  # release entry point. Anything else at that path is a foreign object.
  defp remove_stale_previous(previous) do
    case File.lstat(previous) do
      {:error, :enoent} ->
        :ok

      {:ok, %{type: :directory}} ->
        if File.regular?(Path.join([previous, "bin", "arc"])) do
          case File.rm_rf(previous) do
            {:ok, _} -> :ok
            {:error, reason, _} -> {:error, {:previous_release_removal_failed, reason}}
          end
        else
          {:error, :previous_path_occupied}
        end

      _ ->
        {:error, :previous_path_occupied}
    end
  end

  defp swap(root, previous, candidate) do
    with :ok <- rename(root, previous, :install_root_rename_failed) do
      case File.rename(candidate, root) do
        :ok ->
          :ok

        {:error, reason} ->
          restore(root, previous, reason)
      end
    end
  end

  defp restore(root, previous, reason) do
    case File.rename(previous, root) do
      :ok -> {:error, {:install_rename_failed, reason}}
      {:error, restore_reason} -> {:error, {:install_unrecoverable, root, reason, restore_reason}}
    end
  end

  defp rename(from, to, code) do
    case File.rename(from, to) do
      :ok -> :ok
      {:error, reason} -> {:error, {code, reason}}
    end
  end

  defp verify_entries(entries, version) do
    cond do
      length(entries) > @max_entries -> {:error, :too_many_archive_entries}
      expanded_size(entries) > @max_expanded_bytes -> {:error, :archive_expands_too_large}
      true -> verify_entry_list(entries, version, %{})
    end
  end

  defp verify_entry_list([], version, seen) do
    missing = Enum.reject(@required, &Map.has_key?(seen, &1))

    cond do
      missing != [] -> {:error, {:missing_archive_entries, missing}}
      not version_directory?(seen, version) -> {:error, :missing_release_directory}
      true -> :ok
    end
  end

  defp verify_entry_list([entry | rest], version, seen) do
    name = entry_name(entry)

    cond do
      not safe_path?(name) -> {:error, {:unsafe_archive_path, name}}
      not under_prefix?(name) -> {:error, {:unexpected_archive_path, name}}
      Map.has_key?(seen, name) -> {:error, {:duplicate_archive_path, name}}
      entry_type(entry) not in [:regular, :directory] -> {:error, {:unsafe_archive_entry, name}}
      true -> verify_entry_list(rest, version, Map.put(seen, name, true))
    end
  end

  defp version_directory?(seen, version) do
    directory = "arc/releases/" <> version

    Map.has_key?(seen, directory) or
      Enum.any?(Map.keys(seen), &String.starts_with?(&1, directory <> "/"))
  end

  defp under_prefix?("arc"), do: true
  defp under_prefix?(name), do: String.starts_with?(name, @prefix)

  defp safe_path?(name) do
    Path.type(name) == :relative and name != "" and String.valid?(name) and
      Enum.all?(String.split(name, "/"), &(&1 not in ["", ".", ".."]))
  end

  defp table(path) do
    case :erl_tar.table(String.to_charlist(path), [:compressed, :verbose]) do
      {:ok, entries} -> {:ok, entries}
      {:error, reason} -> {:error, {:invalid_archive, reason}}
    end
  end

  defp entry_name({name, _type, _size, _mtime, _mode, _uid, _gid}) when is_list(name),
    do: List.to_string(name)

  defp entry_type({_name, type, _size, _mtime, _mode, _uid, _gid}), do: type
  defp entry_size({_name, _type, size, _mtime, _mode, _uid, _gid}), do: size
  defp expanded_size(entries), do: entries |> Enum.map(&entry_size/1) |> Enum.sum()

  defp file_size(path) do
    case File.lstat(path) do
      {:ok, %{type: :regular, size: size}} -> {:ok, size}
      {:ok, _} -> {:error, :archive_not_regular}
      {:error, reason} -> {:error, {:archive_unavailable, reason}}
    end
  end

  defp verify_digest(path, expected) do
    case File.open(path, [:read, :binary]) do
      {:ok, device} ->
        try do
          actual =
            device |> digest_device(:crypto.hash_init(:sha256)) |> Base.encode16(case: :lower)

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
      {:error, _reason} -> :crypto.hash_final(:crypto.hash_init(:sha256))
      data -> digest_device(device, :crypto.hash_update(state, data))
    end
  end

  defp expected_version(opts) do
    case Keyword.get(opts, :version) do
      version when is_binary(version) ->
        if Regex.match?(@version_pattern, version),
          do: {:ok, version},
          else: {:error, :invalid_expected_version}

      _ ->
        {:error, :invalid_expected_version}
    end
  end

  defp expected_digest(opts) do
    case Keyword.get(opts, :sha256) do
      digest when is_binary(digest) ->
        if Regex.match?(@hex_pattern, digest), do: {:ok, digest}, else: {:error, :invalid_digest}

      _ ->
        {:error, :invalid_digest}
    end
  end

  defp expected_size(opts) do
    case Keyword.get(opts, :size) do
      size when is_integer(size) and size > 0 -> {:ok, size}
      _ -> {:error, :invalid_expected_size}
    end
  end
end
