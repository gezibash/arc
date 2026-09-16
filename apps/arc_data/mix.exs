defmodule Arc.Data.MixProject do
  use Mix.Project

  def project do
    [
      app: :arc_data,
      version: "0.3.0",
      build_path: "../../_build",
      config_path: "../../config/config.exs",
      deps_path: "../../deps",
      lockfile: "../../mix.lock",
      elixir: "~> 1.19",
      start_permanent: Mix.env() == :prod,
      deps: deps()
    ]
  end

  # Run "mix help compile.app" to learn about applications.
  def application do
    [
      extra_applications: [:crypto, :logger],
      mod: {Arc.Data.Application, []}
    ]
  end

  # Run "mix help deps" to learn about dependencies.
  defp deps do
    [
      {:arc_identity, in_umbrella: true},
      {:arc_control, in_umbrella: true},
      {:toml_elixir, "~> 3.1"}
    ]
  end
end
