defmodule Arc.CLI.Relay do
  @moduledoc """
  `arc relay [--port PORT] [--key NAME]` — start a relay node.

  Listens for inbound TCP connections from Arc agents and routes
  ciphertext packets by destination pubkey. The relay never sees
  plaintext; sessions are E2E encrypted.

  Port defaults to ARC_RELAY_PORT env var, then 7331.
  Relay key defaults to ARC_RELAY_KEY; if unset, an ephemeral key is generated.
  The frame cap comes from the global `--max-frame-bytes` flag or
  ARC_RELAY_MAX_FRAME_BYTES (see `Arc.CLI`). It is unbounded by default.
  """

  alias Arc.Identity
  alias Arc.Identity.KeyStore

  def run(args) do
    {opts, _argv, _invalid} = OptionParser.parse(args, strict: [port: :integer, key: :string])
    port = parse_port_opt(opts)
    {relay_id, key_source} = resolve_relay_identity(opts)

    Application.ensure_all_started(:arc_net)
    {:ok, relay} = Arc.Net.Relay.start_link(port, relay_public_key: relay_id.public_key)
    actual_port = Arc.Net.Relay.get_port(relay)

    pk_hex = Identity.encode_public_key(relay_id)
    pk_short = binary_part(pk_hex, 0, 8) <> "…" <> binary_part(pk_hex, byte_size(pk_hex), -8)

    IO.puts("arc relay listening on port #{actual_port}")
    IO.puts("relay identity: #{pk_short} (#{key_source})")
    IO.puts("relay pubkey: #{pk_hex}")
    Process.sleep(:infinity)
  end

  defp default_port do
    parse_port_string(System.get_env("ARC_RELAY_PORT", "7331")) || 7331
  end

  defp parse_port_opt(opts) do
    case Keyword.fetch(opts, :port) do
      {:ok, port} when is_integer(port) and port > 0 and port < 65_536 -> port
      {:ok, _bad} -> error("invalid --port value")
      :error -> default_port()
    end
  end

  defp parse_port_string(port_str) when is_binary(port_str) do
    case Integer.parse(port_str) do
      {port, ""} when port > 0 and port < 65_536 -> port
      _ -> nil
    end
  end

  defp resolve_relay_identity(opts) do
    key_name =
      case Keyword.get(opts, :key) || System.get_env("ARC_RELAY_KEY") do
        "" -> nil
        value -> value
      end

    case key_name do
      nil ->
        {Identity.generate(), "ephemeral"}

      key ->
        case KeyStore.get(key) do
          {:ok, id} -> {id, "keystore:#{Identity.name(id)}"}
          {:error, :not_found} -> error("relay key '#{key}' not found")
          {:error, :ambiguous} -> error("relay key '#{key}' is ambiguous")
        end
    end
  end

  @spec error(String.t()) :: no_return()
  defp error(msg) do
    IO.puts(:stderr, "error: #{msg}")
    Arc.CLI.Exit.halt(1)
  end
end
