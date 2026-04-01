defmodule Arc.CLI.Apps do
  @moduledoc """
  CLI commands for local ARC provider bundles.
  """

  alias Arc.CLI.ProviderBundle

  def run(args) do
    dispatch(args)
  end

  defp dispatch(["init"]) do
    init_bundle(".")
  end

  defp dispatch(["init", path | _]) do
    init_bundle(path)
  end

  defp dispatch(_) do
    IO.puts("""
    arc apps commands

    Commands:
      apps init [path]       Create a local ARC provider bundle scaffold
    """)
  end

  defp init_bundle(path) do
    case ProviderBundle.init(path) do
      {:ok, files} ->
        IO.puts("Initialized ARC app bundle at #{files.root}")
        IO.puts("")
        IO.puts("Created:")
        IO.puts("  #{files.arcfile}")
        IO.puts("  #{files.manifest}")
        IO.puts("  #{files.runtime}")
        IO.puts("")
        IO.puts("Next:")
        IO.puts("  ARC_KEY=<provider-key> bin/arc serve #{files.root}")
        IO.puts("  Edit manifest.json and run.sh to expose your capability")

      {:error, {:already_exists, path}} ->
        error("apps init failed: #{path} already exists")

      {:error, reason} ->
        error("apps init failed: #{inspect(reason)}")
    end
  end

  defp error(message) do
    IO.puts(:stderr, "error: #{message}")
    System.halt(1)
  end
end
