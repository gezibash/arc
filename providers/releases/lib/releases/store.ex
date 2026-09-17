defmodule Releases.Store do
  @moduledoc false

  @hex ~r/\A[0-9a-f]{64}\z/
  @max_chunk_bytes 256 * 1024

  def channel(root, channel) when channel in ["stable", "beta"] do
    with :ok <- safe_directory(root),
         :ok <- safe_directory(Path.join(root, "channels")) do
      read_regular(Path.join([root, "channels", channel <> ".json"]))
    end
  end

  def channel(_, _), do: {:error, "invalid_request"}

  def chunk(root, "sha256:" <> digest, offset, length)
      when is_integer(offset) and offset >= 0 and is_integer(length) and length > 0 and
             length <= @max_chunk_bytes do
    with true <- Regex.match?(@hex, digest) or {:error, "invalid_request"},
         {:ok, path} <- blob_path(root, digest),
         {:ok, %{size: size}} <- regular_stat(path),
         true <- offset <= size or {:error, "invalid_request"},
         {:ok, bytes} <- read_range(path, offset, min(length, size - offset)) do
      {:ok,
       %{"digest" => "sha256:" <> digest, "offset" => offset, "data" => Base.encode64(bytes)}}
    else
      {:error, :enoent} -> {:error, "not_found"}
      false -> {:error, "invalid_request"}
      {:error, reason} when is_binary(reason) -> {:error, reason}
      {:error, _} -> {:error, "storage_failure"}
    end
  end

  def chunk(_, _, _, _), do: {:error, "invalid_request"}

  def local_channel(root, channel), do: channel(root, channel)

  def local_chunk(root, digest, offset, length),
    do: chunk(root, "sha256:" <> digest, offset, length)

  defp blob_path(root, digest) do
    blob_dir = Path.join(root, "blobs")
    path = Path.join(blob_dir, digest <> ".tar.gz")

    with :ok <- safe_directory(root),
         :ok <- safe_directory(blob_dir),
         true <- Path.dirname(path) == blob_dir or {:error, :invalid_request} do
      {:ok, path}
    else
      {:error, _} = error -> error
      false -> {:error, :invalid_request}
    end
  end

  defp read_regular(path) do
    with {:ok, %{size: size}} <- regular_stat(path),
         true <- size <= @max_chunk_bytes or {:error, :too_large},
         {:ok, bytes} <- File.read(path) do
      {:ok, bytes}
    else
      {:error, :enoent} -> {:error, "not_found"}
      {:error, _} -> {:error, "storage_failure"}
      false -> {:error, "response_too_large"}
    end
  end

  defp read_range(path, offset, length) do
    case File.open(path, [:read, :binary]) do
      {:ok, io} ->
        try do
          with {:ok, _} <- :file.position(io, offset), do: {:ok, IO.binread(io, length) || ""}
        after
          File.close(io)
        end

      error ->
        error
    end
  end

  defp regular_stat(path) do
    case File.lstat(path) do
      {:ok, %{type: :regular} = stat} -> {:ok, stat}
      {:ok, _} -> {:error, :unsafe_file}
      {:error, _} = error -> error
    end
  end

  defp safe_directory(path) do
    case File.lstat(path) do
      {:ok, %{type: :directory}} -> :ok
      {:error, _} -> {:error, "storage_failure"}
      _ -> {:error, "storage_failure"}
    end
  end
end
