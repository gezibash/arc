defmodule Dm.MixProject do
  use Mix.Project

  def project do
    [
      app: :dm,
      version: "0.1.0",
      elixir: "~> 1.19",
      start_permanent: false,
      escript: [main_module: Dm.Main, name: "dm"],
      deps: []
    ]
  end

  def application do
    [extra_applications: [:logger, :crypto]]
  end
end
