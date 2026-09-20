defmodule Arc.CLI.Exit do
  @moduledoc """
  How the CLI stops with a non-zero status.

  Commands call `halt/1` instead of `System.halt/1`. It raises, so every
  `try ... after` on the way out still runs, and `Arc.CLI.main/1` turns the
  exception into the process exit code. Under test, `:arc_cli, :exit_mode`
  is `:return`, and `main/1` returns `{:exit, code}` instead of stopping
  the VM.
  """

  defmodule Error do
    defexception [:code]

    @impl true
    def message(%{code: code}), do: "arc exited with status #{code}"
  end

  @doc "Stop the command with `code`. Never returns."
  @spec halt(non_neg_integer()) :: no_return()
  def halt(code) when is_integer(code) and code >= 0 do
    raise Error, code: code
  end

  @doc false
  def finish(code) do
    case Application.get_env(:arc_cli, :exit_mode, :halt) do
      :return -> {:exit, code}
      _ -> System.halt(code)
    end
  end
end
