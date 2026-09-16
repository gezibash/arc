defmodule Files.Store do
  @moduledoc false
  alias Files.Envelope

  def ensure(root) do
    with :ok <- safe_mkdir(root), :ok <- safe_mkdir(Path.join(root, "objects")), do: :ok
  end

  def put(root, owner, envelope) do
    target = path(root, owner, envelope["id"])

    with :ok <- ensure_owner(root, owner) do
      # Check a retry before quota. A full owner may always retry an already
      # accepted envelope, but never replace it with another value.
      case regular_file?(target) do
        :ok ->
          case File.read(target) do
            {:ok, bytes} -> existing(bytes, envelope)
            _ -> {:error, "storage_failure"}
          end

        {:error, "not_found"} ->
          with :ok <- quota_allows?(root, owner, envelope),
               {:ok, result} <- write_once(target, encode(envelope)) do
            case result do
              :created -> {:ok, %{"id" => envelope["id"]}}
              {:exists, bytes} -> existing(bytes, envelope)
            end
          end

        _ ->
          {:error, "corrupt_storage"}
      end
    end
  end

  def get(root, owner, id) do
    with true <- Envelope.id?(id),
         :ok <- owner_directory?(owner_dir(root, owner)),
         :ok <- regular_file?(path(root, owner, id)),
         {:ok, data} <- File.read(path(root, owner, id)),
         {:ok, %{} = file} <- decode_file(data),
         {:ok, envelope} <- Envelope.validate(file, owner) do
      {:ok, %{"file" => envelope}}
    else
      false -> {:error, "invalid_request"}
      {:error, "not_found"} -> {:error, "not_found"}
      {:error, "corrupt_storage"} -> {:error, "corrupt_storage"}
      _ -> {:error, "corrupt_storage"}
    end
  end

  def list(root, owner, cursor) do
    with true <- is_nil(cursor) or Envelope.id?(cursor),
         {:ok, files} <- list_owner(owner_dir(root, owner)) do
      ids =
        files
        |> Enum.filter(&(String.length(&1) == 69 and String.ends_with?(&1, ".json")))
        |> Enum.map(&String.slice(&1, 0, 64))
        |> Enum.filter(&Envelope.id?/1)
        |> Enum.sort()
        |> Enum.filter(&(is_nil(cursor) or &1 > cursor))
        |> Enum.take(50)

      with {:ok, headers} <- load_headers(root, owner, ids) do
        next = if length(ids) == 50, do: List.last(ids), else: :null
        {:ok, %{"files" => headers, "next" => next}}
      end
    else
      {:error, "corrupt_storage"} -> {:error, "corrupt_storage"}
      _ -> {:error, "invalid_request"}
    end
  end

  defp existing(bytes, envelope) do
    case :json.decode(bytes) do
      existing when existing == envelope -> {:ok, %{"id" => envelope["id"]}}
      _ -> {:error, "conflict"}
    end
  rescue
    _ -> {:error, "conflict"}
  end

  defp quota_allows?(root, owner, envelope) do
    bytes = byte_size(encode(envelope))

    with {:ok, quota} <- Files.Config.quota_bytes(),
         {:ok, used} <- usage(owner_dir(root, owner)) do
      if used + bytes <= quota, do: :ok, else: {:error, "quota_exceeded"}
    end
  end

  defp usage(dir) do
    with :ok <- safe_directory?(dir),
         {:ok, names} <- File.ls(dir) do
      Enum.reduce_while(names, {:ok, 0}, fn name, {:ok, total} ->
        case File.lstat(Path.join(dir, name)) do
          {:ok, %{type: :regular, size: size}} -> {:cont, {:ok, total + size}}
          _ -> {:halt, {:error, "corrupt_storage"}}
        end
      end)
    else
      _ -> {:error, "corrupt_storage"}
    end
  end

  defp write_once(target, content) do
    suffix = :crypto.strong_rand_bytes(16) |> Base.url_encode64(padding: false)
    temp = target <> ".tmp-" <> suffix
    do_write_once(temp, target, content)
  end

  defp do_write_once(temp, target, content) do
    case File.open(temp, [:write, :binary, :exclusive]) do
      {:ok, io} ->
        try do
          :ok = IO.binwrite(io, content)
          :ok = :file.sync(io)
          :ok = File.close(io)

          case File.ln(temp, target) do
            :ok ->
              :ok = File.rm(temp)
              {:ok, :created}

            {:error, :eexist} ->
              :ok = File.rm(temp)

              with :ok <- regular_file?(target),
                   {:ok, bytes} <- File.read(target) do
                {:ok, {:exists, bytes}}
              else
                _ -> {:error, "storage_failure"}
              end

            _ ->
              File.rm(temp)
              {:error, "storage_failure"}
          end
        after
          # `File.close/1` has already run on success. It is harmless for the
          # temporary descriptor to be closed again while unwinding a failure.
          File.close(io)
        end

      _ ->
        {:error, "storage_failure"}
    end
  rescue
    _ ->
      File.rm(temp)
      {:error, "storage_failure"}
  end

  defp ensure_owner(root, owner) do
    with :ok <- safe_mkdir(owner_dir(root, owner)), do: :ok
  end

  defp owner_dir(root, owner), do: Path.join([root, "objects", owner])
  defp path(root, owner, id), do: Path.join(owner_dir(root, owner), id <> ".json")

  defp list_owner(dir) do
    case File.lstat(dir) do
      {:error, :enoent} -> {:ok, []}
      {:ok, %{type: :directory}} -> File.ls(dir)
      _ -> {:error, "corrupt_storage"}
    end
  end

  defp owner_directory?(dir) do
    case File.lstat(dir) do
      {:ok, %{type: :directory}} -> :ok
      {:error, :enoent} -> {:error, "not_found"}
      _ -> {:error, "corrupt_storage"}
    end
  end

  defp safe_mkdir(dir) do
    case File.mkdir_p(dir) do
      :ok ->
        case File.lstat(dir) do
          {:ok, %{type: :directory}} -> :ok
          _ -> {:error, "storage_failure"}
        end

      _ ->
        {:error, "storage_failure"}
    end
  end

  defp safe_directory?(dir) do
    case File.lstat(dir) do
      {:ok, %{type: :directory}} -> :ok
      _ -> {:error, "corrupt_storage"}
    end
  end

  defp regular_file?(path) do
    case File.lstat(path) do
      {:ok, %{type: :regular}} -> :ok
      {:error, :enoent} -> {:error, "not_found"}
      _ -> {:error, "corrupt_storage"}
    end
  end

  defp load_headers(root, owner, ids) do
    Enum.reduce_while(ids, {:ok, []}, fn id, {:ok, headers} ->
      case get(root, owner, id) do
        {:ok, %{"file" => envelope}} -> {:cont, {:ok, [Envelope.headers(envelope) | headers]}}
        _ -> {:halt, {:error, "corrupt_storage"}}
      end
    end)
    |> case do
      {:ok, headers} -> {:ok, Enum.reverse(headers)}
      error -> error
    end
  end

  defp decode_file(data) do
    {:ok, :json.decode(data)}
  rescue
    _ -> {:error, "corrupt_storage"}
  end

  defp encode(value), do: value |> :json.encode() |> IO.iodata_to_binary()
end
