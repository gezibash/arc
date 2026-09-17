defmodule Releases.Config do
  @moduledoc false

  @max_chunk_bytes 256 * 1024
  @max_line_bytes 512 * 1024

  def root,
    do: (System.get_env("RELEASES_ROOT") || "~/.local/share/arc/releases") |> Path.expand()

  def max_chunk_bytes, do: @max_chunk_bytes
  def max_line_bytes, do: @max_line_bytes

  def validate do
    case File.lstat(root()) do
      {:ok, %{type: :directory}} -> :ok
      {:error, :enoent} -> {:error, "release root does not exist"}
      _ -> {:error, "release root is not a directory"}
    end
  end
end
