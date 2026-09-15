defmodule Files.Config do
  @moduledoc false
  def root, do: (System.get_env("FILES_ROOT") || "~/.arc/files") |> Path.expand()

  def quota_bytes do
    case System.get_env("FILES_QUOTA_BYTES") do
      nil -> {:ok, 64 * 1024 * 1024}
      value -> parse_positive(value)
    end
  end

  def validate do
    case quota_bytes() do
      {:ok, _bytes} -> :ok
      {:error, _} = error -> error
    end
  end

  def max_line_bytes, do: 6 * 1024 * 1024
  def max_name_bytes, do: 2_048
  def max_body_bytes, do: 5_592_500

  defp parse_positive(value) do
    case Integer.parse(value) do
      {bytes, ""} when bytes > 0 -> {:ok, bytes}
      _ -> {:error, "invalid FILES_QUOTA_BYTES"}
    end
  end
end
