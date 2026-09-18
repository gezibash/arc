defmodule Arc.CLI.Update do
  @moduledoc false

  alias Arc.CLI.Update.AdminClient
  alias Arc.CLI.Update.SelfUpdate

  @operations ["status", "check", "apply"]
  @switches [
    socket: :string,
    channel: :string,
    publisher: :string,
    replace_publisher: :boolean,
    source: :string,
    relay: :string,
    relay_pubkey: :string,
    format: :string,
    json: :boolean,
    help: :boolean
  ]
  @installation_only [:channel, :publisher, :replace_publisher, :source, :relay, :relay_pubkey]

  def run(["help"]), do: help()

  def run(args) do
    case OptionParser.parse(args, strict: @switches) do
      {opts, operations, []} -> execute(operations, opts)
      _ -> usage_error()
    end
  end

  defp execute(operations, opts) do
    cond do
      opts[:help] -> help()
      is_binary(opts[:socket]) -> service(operations, opts)
      true -> installation(operations, opts)
    end
  end

  # A managed service is administered only through its private socket. The
  # installation options have no meaning there and are refused, not ignored.
  defp service([operation], opts) when operation in @operations do
    if Enum.any?(@installation_only ++ [:format, :json], &Keyword.has_key?(opts, &1)) do
      usage_error()
    end

    case AdminClient.request(opts[:socket], operation) do
      {:ok, document} -> IO.puts(:json.encode(document))
      {:error, reason} -> error("update request failed: " <> error_code(reason))
    end
  end

  defp service(_operations, _opts), do: usage_error()

  defp installation(operations, opts) do
    with {:ok, operation} <- installation_operation(operations),
         {:ok, format} <- format(opts) do
      case SelfUpdate.run(operation, opts) do
        {:ok, document} ->
          print_report(document, format)
          if document["state"] == "blocked", do: Arc.CLI.Exit.halt(1)
          :ok

        {:error, reason} ->
          error(SelfUpdate.describe_error(reason))
      end
    else
      :error -> usage_error()
    end
  end

  defp installation_operation([]), do: {:ok, "apply"}
  defp installation_operation([operation]) when operation in @operations, do: {:ok, operation}
  defp installation_operation(_operations), do: :error

  defp format(opts) do
    format = opts[:format] || if(opts[:json], do: "json", else: "table")

    cond do
      format not in ["table", "json"] -> :error
      opts[:json] && format != "json" -> :error
      true -> {:ok, format}
    end
  end

  defp print_report(document, "json"),
    do: document |> :json.encode() |> IO.iodata_to_binary() |> IO.puts()

  defp print_report(document, "table") do
    IO.puts(String.pad_trailing("Key", 24) <> "Value")
    IO.puts(String.pad_trailing("---", 24) <> "-----")

    for {label, value} <- rows(document), value != nil do
      IO.puts(String.pad_trailing(label, 24) <> value)
    end
  end

  defp rows(document) do
    installed = document["installed"] || %{}

    [
      {"Installation", installed["root"]},
      {"State", document["state"]},
      {"Reason", present(document["reason"])},
      {"Channel", document["channel"]},
      {"Installed", installed["version"]},
      {"Latest", release_label(document["latest"])},
      {"Install", release_label(document["install"])},
      {"Source", present(document["source"])},
      {"Relay", present(document["relay"])},
      {"Identity", present(document["identity"])},
      {"Publisher", present(document["publisher"])},
      {"Previous", present(document["previous"])},
      {"Message", present(document["message"])}
    ]
  end

  defp present(value) when is_binary(value), do: value
  defp present(value) when is_integer(value), do: Integer.to_string(value)
  defp present(_value), do: nil

  defp release_label(%{"version" => version} = release) do
    case release["size"] do
      size when is_integer(size) -> "#{version} (#{size} bytes)"
      _ -> version
    end
  end

  defp release_label(_release), do: nil

  defp error_code(reason) when is_atom(reason), do: Atom.to_string(reason)

  defp error_code(reason) when is_binary(reason) do
    if Regex.match?(~r/\A[a-z_]{1,80}\z/, reason), do: reason, else: "admin_request_failed"
  end

  defp error_code(_), do: "admin_request_failed"

  defp help do
    IO.puts("""
    arc update [check|apply|status] [options]
    arc update status|check|apply --socket PATH

    Without --socket, arc update connects to your joined relay as your active
    key, finds a release provider there, verifies the signed release channel
    against the trusted publisher, and
    replaces this installation when a newer release is available. check reports
    availability without downloading an archive. status prints local update
    settings without using the network. The replaced release is kept beside
    the installation as <root>.previous until the next update.

    Options:
      --publisher KEY        Trust and remember this release publisher (hex public key)
      --replace-publisher    Allow --publisher to replace a previously trusted key
      --channel stable|beta  Select and remember the release channel (default stable)
      --source URI           Use one releases+arc://KEY/releases provider instead of searching
      --relay host:port      Override the joined relay for this run
      --relay-pubkey KEY     Pin the relay public key
      --format table|json    Output format (--json is an alias for --format json)

    With --socket, the command targets one locally managed relay service. Output
    is JSON. check reports channel availability without downloading an archive;
    apply starts an operator-requested hot update; inspect status for completion.
    No command restarts an incompatible service or changes remote relay policy.
    Configure channel, immutable build pin, publisher and source in the service
    file. An interrupted installation blocks retries until operator reconciliation.
    """)
  end

  @spec usage_error() :: no_return()
  defp usage_error do
    error(
      "usage: arc update [check|apply|status] [--publisher KEY] [--channel stable|beta] " <>
        "[--source URI] [--format table|json], or arc update status|check|apply --socket PATH"
    )
  end

  @spec error(String.t()) :: no_return()
  defp error(message) do
    IO.puts(:stderr, "error: " <> message)
    Arc.CLI.Exit.halt(1)
  end
end
