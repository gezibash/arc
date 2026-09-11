defmodule Journal.MixProject do
  use Mix.Project

  def project do
    [
      app: :journal,
      version: "0.1.0",
      elixir: "~> 1.19",
      start_permanent: false,
      escript: [main_module: Journal.Main, name: "journal"],
      deps: deps()
    ]
  end

  def application do
    [extra_applications: [:logger, :crypto]]
  end

  defp deps do
    [{:yaml_elixir, "~> 2.11"}]
  end
end
