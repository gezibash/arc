defmodule Releases.Main do
  @moduledoc false
  alias Releases.{Config, Stdio}

  def main(_args) do
    case Config.validate() do
      :ok ->
        Stdio.loop(Config.root())

      {:error, message} ->
        IO.puts(:stderr, message)
        System.halt(1)
    end
  end
end
