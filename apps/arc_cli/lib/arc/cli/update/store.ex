defmodule Arc.CLI.Update.Store do
  @moduledoc false

  @names ~w(checkpoint journal)
  @limit 1_048_576

  def read(root, name) when name in @names do
    path = Path.join(root, name <> ".json")

    case File.lstat(path) do
      {:error, :enoent} -> {:ok, %{}}
      {:ok, %{type: :regular, size: size}} when size <= @limit -> decode(path)
      _ -> {:error, :invalid_update_state}
    end
  end

  def write(root, name, document) when name in @names and is_map(document) do
    path = Path.join(root, name <> ".json")
    temporary = path <> "." <> Base.encode16(:crypto.strong_rand_bytes(12), case: :lower)
    bytes = IO.iodata_to_binary(:json.encode(document))

    with true <- byte_size(bytes) <= @limit or {:error, :update_state_too_large},
         {:ok, file} <- File.open(temporary, [:write, :binary, :exclusive]) do
      result =
        try do
          with :ok <- File.chmod(temporary, 0o600),
               :ok <- IO.binwrite(file, bytes) do
            :file.sync(file)
          end
        after
          File.close(file)
        end

      case result do
        :ok ->
          with :ok <- File.rename(temporary, path), do: sync_directory(root)

        error ->
          error
      end
    end
  end

  defp sync_directory(root) do
    with {:ok, directory} <- :file.open(String.to_charlist(root), [:read, :raw, :directory]) do
      result = :file.sync(directory)
      closed = :file.close(directory)
      if result == :ok, do: closed, else: result
    end
  end

  defp decode(path) do
    with {:ok, bytes} <- File.read(path),
         document when is_map(document) <- :json.decode(bytes) do
      {:ok, document}
    else
      _ -> {:error, :invalid_update_state}
    end
  rescue
    _ -> {:error, :invalid_update_state}
  end
end
