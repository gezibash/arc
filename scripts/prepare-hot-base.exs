#!/usr/bin/env elixir

# Initializes a *stopped, copied* OTP release root for ARC's future in-place
# updater. This is intentionally not wired into `arc` and refuses roots that
# already have release-handler state. It is a bootstrap operation, never an
# action against a running deployment.

defmodule Arc.HotRelease.Base do
  @release_name "arc_runtime"

  def run(argv) do
    with {:ok, options} <- parse(argv),
         :ok <- ensure_not_current_runtime(options.root),
         {:ok, release} <- release(options.root),
         :ok <- ensure_uninitialized(release.release_dir),
         :ok <- create_releases(options.root, release),
         :ok <- write_metadata(release, options) do
      IO.puts(Path.join(release.release_dir, "arc-build.json"))
    else
      {:error, reason} -> raise "hot base was not prepared: #{inspect(reason)}"
    end
  end

  defp parse(argv) do
    {opts, rest, invalid} =
      OptionParser.parse(argv,
        strict: [root: :string, build: :string, os: :string, arch: :string]
      )

    with true <- rest == [] and invalid == [],
         {:ok, root} <- required_directory(opts, :root),
         {:ok, build} <- required_string(opts, :build),
         {:ok, os} <- required_string(opts, :os),
         {:ok, arch} <- required_string(opts, :arch) do
      {:ok, %{root: root, build: build, os: os, arch: arch}}
    else
      _ -> {:error, :usage}
    end
  end

  defp required_directory(opts, key) do
    with {:ok, path} <- required_string(opts, key),
         root = Path.expand(path),
         true <- File.dir?(root) do
      {:ok, root}
    else
      false -> {:error, {:not_a_directory, key}}
      error -> error
    end
  end

  defp required_string(opts, key) do
    case opts[key] do
      value when is_binary(value) and value != "" -> {:ok, value}
      _ -> {:error, {:missing_option, key}}
    end
  end

  defp ensure_not_current_runtime(root) do
    if Path.expand(List.to_string(:code.root_dir())) == root do
      {:error, :refusing_to_prepare_the_running_runtime_root}
    else
      :ok
    end
  end

  defp release(root) do
    start_erl = Path.join(root, "releases/start_erl.data")

    with {:ok, contents} <- File.read(start_erl),
         [runtime, version] <- String.split(String.trim(contents), ~r/\s+/, parts: 2),
         release_dir = Path.join(root, "releases/#{version}"),
         rel_file = Path.join(release_dir, "#{@release_name}.rel"),
         {:ok, [{:release, {name, rel_version}, {:erts, rel_runtime}, _apps}]} <-
           :file.consult(String.to_charlist(rel_file)),
         true <-
           release_name?(name) and List.to_string(rel_version) == version and
             List.to_string(rel_runtime) == runtime do
      {:ok, %{version: version, runtime: runtime, release_dir: release_dir, rel_file: rel_file}}
    else
      false -> {:error, :release_and_start_erl_do_not_match}
      _ -> {:error, :invalid_release_root}
    end
  end

  defp release_name?(:arc_runtime), do: true
  defp release_name?(name) when is_list(name), do: List.to_string(name) == @release_name
  defp release_name?(_name), do: false

  defp ensure_uninitialized(release_dir) do
    releases = Path.expand(Path.join(release_dir, ".."))

    cond do
      File.exists?(Path.join(releases, "RELEASES")) ->
        {:error, :release_handler_state_already_exists}

      File.exists?(Path.join(release_dir, "arc-build.json")) ->
        {:error, :build_metadata_already_exists}

      true ->
        :ok
    end
  end

  defp create_releases(root, release) do
    releases_dir = Path.join(root, "releases")

    case :release_handler.create_RELEASES(
           String.to_charlist(root),
           String.to_charlist(releases_dir),
           String.to_charlist(release.rel_file),
           []
         ) do
      :ok -> :ok
      {:error, reason} -> {:error, {:could_not_create_RELEASES, reason}}
    end
  end

  defp write_metadata(release, options) do
    metadata = %{
      "version" => release.version,
      "build" => options.build,
      "runtime" => release.runtime,
      "platform" => %{"os" => options.os, "arch" => options.arch}
    }

    File.write(Path.join(release.release_dir, "arc-build.json"), :json.encode(metadata))
  end
end

Arc.HotRelease.Base.run(System.argv())
