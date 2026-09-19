defmodule Arc.Identity.MixProject do
  use Mix.Project

  def project do
    [
      app: :arc_identity,
      version: "0.5.1",
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
      mod: {Arc.Identity.Application, []}
    ]
  end

  # Run "mix help deps" to learn about dependencies.
  defp deps do
    [
      {:b3, "~> 0.1.0"},
      {:toml_elixir, "~> 3.1"}
    ]
  end
end
