defmodule Releases.MixProject do
  use Mix.Project

  def project do
    [
      app: :releases,
      version: "0.1.0",
      elixir: "~> 1.19",
      start_permanent: false,
      escript: [main_module: Releases.Main, name: "releases"],
      deps: []
    ]
  end

  def application, do: [extra_applications: [:crypto]]
end
