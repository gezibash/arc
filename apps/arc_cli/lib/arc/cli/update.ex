defmodule Arc.CLI.Update do
  @moduledoc false

  def run(["--help"]), do: help()
  def run(["help"]), do: help()

  def run(args) do
    case OptionParser.parse(args, strict: [socket: :string]) do
      {[socket: path], [operation], []} when operation in ["status", "check", "apply"] ->
        case Arc.CLI.Update.AdminClient.request(path, operation) do
          {:ok, document} -> IO.puts(:json.encode(document))
          {:error, reason} -> error("update request failed: " <> error_code(reason))
        end

      _ ->
        error("usage: arc update status|check|apply --socket PATH")
    end
  end

  defp error_code(reason) when is_atom(reason), do: Atom.to_string(reason)

  defp error_code(reason) when is_binary(reason) do
    if Regex.match?(~r/\A[a-z_]{1,80}\z/, reason), do: reason, else: "admin_request_failed"
  end

  defp error_code(_), do: "admin_request_failed"

  defp help do
    IO.puts("""
    arc update status|check|apply --socket PATH

    Targets one locally managed service. Output is JSON.
    check reports channel availability without downloading an archive.
    apply starts an operator-requested hot update; inspect status for completion.
    No command restarts an incompatible service or changes remote relay policy.
    Configure channel, immutable build pin, publisher and source in the service file.
    An interrupted installation blocks retries until operator reconciliation.
    """)
  end

  @spec error(String.t()) :: no_return()
  defp error(message) do
    IO.puts(:stderr, "error: " <> message)
    Arc.CLI.Exit.halt(1)
  end
end
