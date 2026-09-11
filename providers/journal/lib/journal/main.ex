defmodule Journal.Main do
  @moduledoc """
  Escript entry point. Starts the supervisor, then runs the stdio loop
  until stdin closes.
  """

  def main(_args) do
    root = Journal.Config.root()
    :ok = Journal.Store.ensure(root)

    children = [
      {Journal.Index, root},
      {Journal.Push, root}
    ]

    {:ok, _sup} = Supervisor.start_link(children, strategy: :one_for_one)
    Journal.Stdio.loop(root)
  end
end
