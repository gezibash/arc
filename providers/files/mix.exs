defmodule Files.MixProject do
  use Mix.Project

  def project do
    [
      app: :files,
      version: "0.1.0",
      elixir: "~> 1.19",
      start_permanent: false,
      escript: [main_module: Files.Main, name: "files"],
      deps: []
    ]
  end

  def application, do: [extra_applications: [:crypto]]
end
