defmodule Arc.Net.MixProject do
  use Mix.Project

  def project do
    [
      app: :arc_net,
      version: "0.4.0",
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
      extra_applications: [:crypto, :ssl, :public_key, :logger],
      mod: {Arc.Net.Application, []}
    ]
  end

  # Run "mix help deps" to learn about dependencies.
  defp deps do
    [
      {:arc_identity, in_umbrella: true},
      {:arc_data, in_umbrella: true},
      {:telemetry, "~> 1.3"}
    ]
  end
end
