defmodule Arc.Control.Application do
  @moduledoc false

  use Application

  @impl true
  def start(_type, _args) do
    children = [
      Arc.Control.Local
    ]

    opts = [strategy: :one_for_one, name: Arc.Control.Supervisor]
    Supervisor.start_link(children, opts)
  end
end
