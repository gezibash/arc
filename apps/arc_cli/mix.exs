defmodule Arc.CLI.MixProject do
  use Mix.Project

  def project do
    [
      app: :arc_cli,
      version: "0.3.2",
      build_path: "../../_build",
      config_path: "../../config/config.exs",
      deps_path: "../../deps",
      lockfile: "../../mix.lock",
      elixir: "~> 1.19",
      start_permanent: Mix.env() == :prod,
      escript: escript(),
      deps: deps()
    ]
  end

  defp escript do
    [main_module: Arc.CLI, name: "arc", path: "../../bin/arc"]
  end

  # Run "mix help compile.app" to learn about applications.
  def application do
    [
      extra_applications: [:logger]
    ]
  end

  # Run "mix help deps" to learn about dependencies.
  defp deps do
    [
      {:arc_identity, in_umbrella: true},
      {:arc_control, in_umbrella: true},
      {:arc_data, in_umbrella: true},
      {:arc_mcp, in_umbrella: true},
      {:arc_net, in_umbrella: true},
      {:arc_storage, in_umbrella: true},
      {:toml_elixir, "~> 3.1"}
    ]
  end
end
