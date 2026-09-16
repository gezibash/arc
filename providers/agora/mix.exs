defmodule Agora.MixProject do
  use Mix.Project

  def project do
    [
      app: :agora,
      version: "0.1.0",
      elixir: "~> 1.19",
      start_permanent: false,
      escript: [main_module: Agora.Main, name: "agora"],
      deps: []
    ]
  end

  def application, do: [extra_applications: [:crypto]]
end
