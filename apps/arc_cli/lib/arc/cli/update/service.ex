defmodule Arc.CLI.Update.Service do
  @moduledoc false

  def run(["--help"]), do: help()
  def run(["help"]), do: help()

  def run(_) do
    IO.puts(
      :stderr,
      "error: service start requires the native release entrypoint; see arc service --help"
    )

    Arc.CLI.Exit.halt(1)
  end

  defp help do
    IO.puts("""
    arc service start --config /absolute/path/service.json

    Runs a relay under the native release's application supervision tree.
    The service file chooses a persistent relay key and local update policy.
    This is an explicit bootstrap operation; existing arc relay processes are unchanged.
    See docs/updates/OPERATIONS.md for configuration and packaging requirements.
    """)
  end
end
