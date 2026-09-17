defmodule Arc.CLI.Status do
  @moduledoc "Queries the configured relay through ARC's own control connection."

  @usage "usage: arc status [--format table|json] [--json] [--relay host:port] [--relay-pubkey KEY]"
  @options [
    help: :boolean,
    check: :boolean,
    json: :boolean,
    format: :string,
    relay: :string,
    relay_pubkey: :string
  ]

  def run(args) do
    case OptionParser.parse(args, strict: @options) do
      {opts, [], []} -> execute(opts)
      _ -> usage_error()
    end
  end

  defp execute(opts) do
    format = opts[:format] || if(opts[:json], do: "json", else: "table")

    cond do
      opts[:help] ->
        IO.puts(@usage)

      format not in ["table", "json"] ->
        usage_error()

      opts[:json] && format != "json" ->
        usage_error()

      true ->
        report = query(opts)
        print_report(report, format)
        if report["state"] == "error", do: Arc.CLI.Exit.halt(1)
    end
  end

  defp query(opts) do
    case Arc.CLI.RelaySettings.resolve(opts) do
      {:ok, %{relay: {_, _} = relay, relay_pubkey: pin}} when is_binary(pin) ->
        fetch(relay, pin)

      {:ok, %{relay: nil}} ->
        error("Join a relay or set --relay or ARC_RELAY to host:port.")

      {:ok, %{relay: {_, _}, relay_pubkey: nil}} ->
        error("Set --relay-pubkey or ARC_RELAY_PUBKEY to a valid relay public key.")

      {:error, :invalid_relay_config} ->
        error(
          "Relay settings are invalid; run arc join again or provide --relay and --relay-pubkey."
        )
    end
  end

  defp fetch({host, port}, pin) do
    result =
      case Arc.Net.RelayStatus.fetch(host, port, pin) do
        {:ok, status} -> status
        {:error, reason} -> error(reason_label(reason))
      end

    Map.put(result, "address", "#{host}:#{port}")
  end

  defp error(message), do: %{"state" => "error", "message" => message}

  defp reason_label(:unsupported),
    do: "This relay does not support status queries; upgrade the relay."

  defp reason_label(:timeout), do: "Relay status query timed out."
  defp reason_label(:econnrefused), do: "Relay connection refused."
  defp reason_label(:nxdomain), do: "Relay host not found."

  defp reason_label(:relay_pubkey_mismatch),
    do: "Relay public key does not match the configured pin."

  defp reason_label(_),
    do: "Relay status unavailable; check the address, public-key pin, and relay version."

  defp print_report(report, "json"),
    do: report |> :json.encode() |> IO.iodata_to_binary() |> IO.puts()

  defp print_report(report, "table") do
    IO.puts(String.pad_trailing("Key", 24) <> "Value")
    IO.puts(String.pad_trailing("---", 24) <> "-----")

    for {field, label} <- [
          {"address", "Relay"},
          {"state", "State"},
          {"role", "Role"},
          {"version", "Version"},
          {"public_key", "Public key"},
          {"uptime_seconds", "Uptime (seconds)"},
          {"federation_transit", "Federation transit"},
          {"message", "Error"}
        ],
        Map.has_key?(report, field) do
      IO.puts(String.pad_trailing(label, 24) <> to_string(report[field]))
    end
  end

  @spec usage_error() :: no_return()
  defp usage_error do
    IO.puts(:stderr, @usage)
    Arc.CLI.Exit.halt(1)
  end
end
