defmodule Arc.CLI.Update.Publisher do
  @moduledoc """
  Operator-side publication of signed channel metadata. Signing identities stay
  outside the read-only provider root. Archives must already exist there under
  their content digest; publication never downloads or executes release bytes.
  """
  alias Arc.CLI.Update.Manifest
  alias Arc.Identity

  @limit 262_144

  def publish(root, identity, unsigned) do
    with :ok <- directory(root),
         :ok <- directory(Path.join(root, "channels")),
         :ok <- directory(Path.join(root, "blobs")),
         {:ok, signed} <- Manifest.sign(identity, unsigned),
         :ok <- verify(signed, identity, System.system_time(:second)),
         bytes = IO.iodata_to_binary(:json.encode(signed)),
         true <- byte_size(bytes) <= @limit or {:error, :channel_too_large},
         lock = Path.join(root, ".publish.lock"),
         {:ok, io} <- File.open(lock, [:write, :exclusive]) do
      try do
        path = Path.join([root, "channels", unsigned["channel"] <> ".json"])

        with :ok <- previous(path, signed, identity),
             :ok <- archives(root, unsigned["releases"]) do
          replace(path, bytes)
        end
      after
        File.close(io)
        File.rm(lock)
      end
    else
      {:error, _} = error -> error
    end
  end

  defp verify(document, identity, now, options \\ []) do
    case Manifest.verify(
           document,
           [
             expected_publisher: Identity.encode_public_key(identity),
             expected_channel: document["channel"],
             now: now
           ] ++ options
         ) do
      {:ok, _} -> :ok
      error -> error
    end
  end

  defp previous(path, signed, identity) do
    case File.lstat(path) do
      {:error, :enoent} ->
        :ok

      {:ok, %{type: :regular, size: size}} when size <= @limit ->
        validate_previous(path, signed, identity)

      _ ->
        {:error, :unsafe_channel}
    end
  end

  defp validate_previous(path, signed, identity) do
    with {:ok, bytes} <- File.read(path),
         {:ok, old} <- decode(bytes),
         {:ok, verified} <-
           Manifest.verify(old,
             expected_publisher: Identity.encode_public_key(identity),
             expected_channel: signed["channel"],
             now: 0
           ),
         true <- signed["sequence"] > old["sequence"] or {:error, :sequence_not_increased},
         :ok <- immutable_builds(old["releases"], signed["releases"]) do
      verify(signed, identity, System.system_time(:second),
        last_sequence: old["sequence"],
        last_digest: verified.digest
      )
    end
  end

  defp immutable_builds(old, releases) do
    conflict =
      Enum.any?(old, fn prior ->
        case Enum.find(releases, fn release ->
               prior["build"] == release["build"] and prior["platform"] == release["platform"]
             end) do
          nil ->
            true

          release ->
            Map.drop(prior, ["withdrawn", "eligible"]) !=
              Map.drop(release, ["withdrawn", "eligible"])
        end
      end)

    if conflict, do: {:error, :immutable_build_changed}, else: :ok
  end

  defp archives(root, releases) do
    releases
    |> Enum.flat_map(&[Map.take(&1, ["sha256", "size"]) | List.wrap(&1["install"])])
    |> Enum.uniq()
    |> Enum.reduce_while(:ok, fn release, :ok ->
      path = Path.join([root, "blobs", release["sha256"] <> ".tar.gz"])

      case archive(path, release) do
        :ok -> {:cont, :ok}
        error -> {:halt, error}
      end
    end)
  end

  defp archive(path, release) do
    with {:ok, %{type: :regular, size: size}} <- File.lstat(path),
         true <- size == release["size"] or {:error, :archive_size_mismatch} do
      digest =
        path
        |> File.stream!(262_144)
        |> Enum.reduce(
          :crypto.hash_init(:sha256),
          fn bytes, hash -> :crypto.hash_update(hash, bytes) end
        )
        |> :crypto.hash_final()
        |> Base.encode16(case: :lower)

      if digest == release["sha256"], do: :ok, else: {:error, :archive_digest_mismatch}
    else
      {:ok, _} -> {:error, :unsafe_archive}
      {:error, _} = error -> error
    end
  rescue
    _ in File.Error -> {:error, :archive_read_failed}
  end

  defp replace(path, bytes) do
    temp = path <> "." <> Base.encode16(:crypto.strong_rand_bytes(12), case: :lower)

    with {:ok, io} <- File.open(temp, [:write, :binary, :exclusive]) do
      result =
        try do
          with :ok <- IO.binwrite(io, bytes), do: :file.sync(io)
        after
          File.close(io)
        end

      with :ok <- result,
           :ok <- File.rename(temp, path),
           {:ok, dir} <-
             :file.open(String.to_charlist(Path.dirname(path)), [:read, :raw, :directory]) do
        try do
          :file.sync(dir)
        after
          :file.close(dir)
        end
      end
    end
  end

  defp directory(path) do
    case File.lstat(path) do
      {:ok, %{type: :directory}} -> :ok
      _ -> {:error, :unsafe_directory}
    end
  end

  defp decode(bytes) do
    case :json.decode(bytes) do
      value when is_map(value) -> {:ok, value}
      _ -> {:error, :invalid_channel}
    end
  rescue
    _ -> {:error, :invalid_channel}
  end
end
