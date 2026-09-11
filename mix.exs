defmodule Arc.MixProject do
  use Mix.Project

  def project do
    [
      apps_path: "apps",
      version: "0.1.0",
      start_permanent: Mix.env() == :prod,
      deps: deps(),
      aliases: aliases(),
      dialyzer: [
        plt_add_apps: [:mix, :ex_unit],
        plt_core_path: "_build/plts",
        plt_local_path: "_build/plts"
      ]
    ]
  end

  def cli do
    [preferred_envs: [check: :test]]
  end

  # Dependencies listed here are available only for this
  # project and cannot be accessed from applications inside
  # the apps folder.
  #
  # Run "mix help deps" for examples and options.
  defp deps do
    [
      {:credo, "~> 1.7", only: [:dev, :test], runtime: false},
      {:dialyxir, "~> 1.4", only: [:dev, :test], runtime: false},
      {:mix_audit, "~> 2.1", only: [:dev, :test], runtime: false}
    ]
  end

  defp aliases do
    [
      lint: [
        "format --check-formatted",
        "compile --warnings-as-errors --all-warnings",
        "credo --strict",
        "xref graph --label compile-connected --fail-above 0"
      ],
      audit: ["deps.unlock --check-unused", &hex_audit/1, "deps.audit"],
      check: ["lint", "audit", &dialyzer/1, "test"]
    ]
  end

  # Hex tasks are not loaded inside an alias, so shell out once.
  defp hex_audit(_args), do: mix(["hex.audit"], "dev")

  # Dialyzer runs under :dev so `mix check` can still run tests under :test.
  defp dialyzer(_args), do: mix(["dialyzer"], "dev")

  defp mix(args, env) do
    case System.cmd("mix", args, env: [{"MIX_ENV", env}], into: IO.stream()) do
      {_, 0} -> :ok
      {_, status} -> Mix.raise("mix #{Enum.join(args, " ")} failed with status #{status}")
    end
  end
end
