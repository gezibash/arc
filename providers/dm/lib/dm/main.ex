defmodule Dm.Main do
  @moduledoc "Escript entry point. Runs the stdio loop until stdin closes."

  def main(_args) do
    root = Dm.Config.root()
    :ok = Dm.Store.ensure(root)
    Dm.Stdio.loop(root)
  end
end
