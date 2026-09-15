defmodule Agora.Main do
  @moduledoc false

  def main(_args) do
    board = System.get_env("ARC_PUBLIC_KEY") || ""

    with :ok <- Agora.Config.validate(board),
         root = Agora.Config.root(board),
         {:ok, lock} <- Agora.Store.acquire_lock(root) do
      try do
        Agora.Stdio.loop(root, board)
      after
        Agora.Store.release_lock(lock)
      end
    else
      {:error, message} ->
        IO.puts(:stderr, message)
        System.halt(1)
    end
  end
end
