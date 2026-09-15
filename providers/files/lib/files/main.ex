defmodule Files.Main do
  @moduledoc false
  def main(_args) do
    case Files.Config.validate() do
      :ok ->
        root = Files.Config.root()
        :ok = Files.Store.ensure(root)
        Files.Stdio.loop(root)

      {:error, message} ->
        IO.puts(:stderr, message)
        System.halt(1)
    end
  end
end
