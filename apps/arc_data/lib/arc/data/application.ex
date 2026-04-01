defmodule Arc.Data.Application do
  @moduledoc false

  use Application

  @impl true
  def start(_type, _args) do
    children = [
      {Registry, keys: :unique, name: Arc.Data.AgentRegistry}
    ]

    opts = [strategy: :one_for_one, name: Arc.Data.Supervisor]
    Supervisor.start_link(children, opts)
  end
end
