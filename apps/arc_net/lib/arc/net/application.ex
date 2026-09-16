defmodule Arc.Net.Application do
  # See https://hexdocs.pm/elixir/Application.html
  # for more information on OTP Applications
  @moduledoc false

  use Application

  @impl true
  def start(_type, _args) do
    children = [
      {Task.Supervisor, name: Arc.Net.TaskSupervisor},
      {Registry, keys: :unique, name: Arc.Net.TransportRegistry},
      {Registry, keys: :unique, name: Arc.Net.DirectRegistry},
      {DynamicSupervisor, strategy: :one_for_one, name: Arc.Net.TransportSupervisor},
      Arc.Net.TransportManager
    ]

    # See https://hexdocs.pm/elixir/Supervisor.html
    # for other strategies and supported options
    opts = [strategy: :one_for_one, name: Arc.Net.Supervisor]
    Supervisor.start_link(children, opts)
  end
end
