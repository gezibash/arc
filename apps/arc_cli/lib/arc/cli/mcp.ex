defmodule Arc.CLI.MCP do
  @moduledoc """
  Run ARC's mounted-capability MCP server over local Streamable HTTP.
  """

  alias Arc.MCP.HTTPServer

  def run(args) do
    {host, args} = pop_opt(args, "--host")
    {port_string, args} = pop_opt(args, "--port")
    {relay_pubkey, args} = pop_opt(args, "--relay-pubkey")
    {relay_addr, clean_args} = pop_opt(args, "--relay")

    opts = [
      host: host || "127.0.0.1",
      port: parse_port(port_string) || 5004,
      relay: relay_addr,
      relay_pubkey: relay_pubkey
    ]

    dispatch(clean_args, opts)
  end

  defp dispatch([task | _], opts) when is_binary(task) and task != "" do
    relay_addr =
      case Keyword.get(opts, :relay) do
        nil -> Arc.Net.relay_address()
        addr -> Arc.Net.relay_address_from(addr)
      end

    relay_pubkey_pin = resolve_relay_pubkey_pin(opts)

    {:ok, server} =
      HTTPServer.start_link(
        task: task,
        host: opts[:host],
        port: opts[:port],
        relay: relay_addr,
        relay_pubkey: relay_pubkey_pin
      )

    IO.puts("MCP listening on #{HTTPServer.url(server)} for task '#{task}'")

    IO.puts(
      "Authenticate initialize requests with x-arc-public-key, x-arc-timestamp, x-arc-nonce, and x-arc-signature headers."
    )

    Process.sleep(:infinity)
  end

  defp dispatch(_, _opts) do
    IO.puts(
      :stderr,
      "usage: arc mcp <task> [--host HOST] [--port PORT] [--relay host:port] [--relay-pubkey <key>]"
    )

    Arc.CLI.Exit.halt(1)
  end

  defp resolve_relay_pubkey_pin(opts) do
    relay_pubkey_opt = Keyword.get(opts, :relay_pubkey)

    relay_pubkey_pin =
      case relay_pubkey_opt do
        nil -> Arc.Net.relay_pubkey()
        value -> Arc.Net.relay_pubkey_from(value)
      end

    cond do
      relay_pubkey_opt != nil and relay_pubkey_pin == nil ->
        error("invalid --relay-pubkey (expected 32-byte hex or base64)")

      relay_pubkey_opt == nil and System.get_env("ARC_RELAY_PUBKEY") != nil and
          relay_pubkey_pin == nil ->
        error("invalid ARC_RELAY_PUBKEY (expected 32-byte hex or base64)")

      true ->
        relay_pubkey_pin
    end
  end

  defp parse_port(nil), do: nil

  defp parse_port(value) do
    case Integer.parse(value) do
      {port, ""} when port >= 0 and port <= 65_535 -> port
      _ -> error("invalid --port '#{value}'")
    end
  end

  defp pop_opt(args, flag), do: pop_opt(args, flag, [])

  defp pop_opt([flag, value | rest], flag, acc) do
    {value, Enum.reverse(acc) ++ rest}
  end

  defp pop_opt([h | rest], flag, acc) do
    pop_opt(rest, flag, [h | acc])
  end

  defp pop_opt([], _flag, acc), do: {nil, Enum.reverse(acc)}

  @spec error(String.t()) :: no_return()
  defp error(msg) do
    IO.puts(:stderr, "error: #{msg}")
    Arc.CLI.Exit.halt(1)
  end
end
