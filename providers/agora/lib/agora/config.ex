defmodule Agora.Config do
  @moduledoc false

  @default_max_posts 10_000

  def root(board),
    do: (System.get_env("AGORA_ROOT") || "~/.local/share/arc/agora/#{board}") |> Path.expand()

  def max_line_bytes, do: 64 * 1024
  def max_posts, do: env_positive("AGORA_MAX_POSTS", @default_max_posts)

  def validate(board) do
    with true <- Agora.Post.key?(board),
         {:ok, _} <- max_posts() do
      :ok
    else
      false -> {:error, "invalid arc_public_key"}
      {:error, _} = error -> error
    end
  end

  defp env_positive(name, default) do
    case System.get_env(name) do
      nil ->
        {:ok, default}

      value ->
        case Integer.parse(value) do
          {number, ""} when number > 0 -> {:ok, number}
          _ -> {:error, "invalid #{name}"}
        end
    end
  end
end
