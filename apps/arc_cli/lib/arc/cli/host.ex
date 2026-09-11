defmodule Arc.CLI.Host do
  @moduledoc """
  Run and inspect the local ARC host service.
  """

  alias Arc.Host.Client
  alias Arc.Host.Service

  def run(args) do
    {socket_path, args} = pop_opt(args, "--socket")
    {relay_pubkey, args} = pop_opt(args, "--relay-pubkey")
    {relay_addr, clean_args} = pop_opt(args, "--relay")

    opts = [
      socket_path: socket_path || Service.default_socket_path(),
      relay: relay_addr,
      relay_pubkey: relay_pubkey
    ]

    dispatch(clean_args, opts)
  end

  defp dispatch(["start" | _], opts) do
    socket_path = opts[:socket_path]
    token_path = Service.admin_token_path(socket_path)

    case Client.request(socket_path, "status") do
      {:ok, _result} ->
        error("host already running at #{socket_path}")

      {:error, _reason} ->
        relay = relay_address(opts[:relay])
        relay_pubkey = resolve_relay_pubkey_pin(opts[:relay_pubkey])

        {:ok, pid} =
          Service.start_link(
            socket_path: socket_path,
            relay: relay,
            relay_pubkey: relay_pubkey
          )

        ref = Process.monitor(pid)

        IO.puts("ARC host listening on #{socket_path}")
        IO.puts("Admin token: #{token_path}")

        case relay do
          {host, port} ->
            IO.puts("Relay: #{List.to_string(host)}:#{port}")

          nil ->
            IO.puts("Relay: local only")
        end

        IO.puts("Press Ctrl+C to stop.")

        receive do
          {:DOWN, ^ref, :process, _pid, _reason} ->
            :ok
        end
    end
  end

  defp dispatch(["status" | _], opts) do
    case Client.request(opts[:socket_path], "status") do
      {:ok, result} ->
        IO.puts("Socket: #{result["socket_path"]}")
        IO.puts("Admin token: #{result["admin_token_path"]}")
        IO.puts("Protocol: v#{result["protocol_version"]}")
        IO.puts("Started: #{result["started_at"]}")
        IO.puts("Connections: #{result["connections"]}")
        IO.puts("Identities: #{result["identity_count"]}")

        case result["identities"] || [] do
          [] -> :ok
          identities -> IO.puts("Loaded: #{Enum.join(identities, ", ")}")
        end

        case result["relay"] do
          %{"host" => host, "port" => port} ->
            IO.puts("Relay: #{host}:#{port}")

          _ ->
            IO.puts("Relay: local only")
        end

      {:error, _reason} ->
        error("host is not running")
    end
  end

  defp dispatch(["stop" | _], opts) do
    with {:ok, admin_token} <- Service.read_admin_token(opts[:socket_path]),
         {:ok, _result} <- Client.request(opts[:socket_path], "shutdown", %{}, token: admin_token) do
      IO.puts("Stopping ARC host")
    else
      {:error, :enoent} ->
        error("host is not running")

      {:error, {"unauthorized", _message}} ->
        error("host admin token is invalid")

      {:error, {"token_expired", _message}} ->
        error("host admin token has expired")

      {:error, _reason} ->
        error("host is not running")
    end
  end

  defp dispatch(["token", "issue" | args], opts) do
    {identity, args} = pop_opt(args, "--identity")
    {ttl, args} = pop_opt(args, "--ttl")
    {label, args} = pop_opt(args, "--label")
    {scopes, _rest} = pop_multi_opt(args, "--scope")

    with {:ok, admin_token} <- Service.read_admin_token(opts[:socket_path]),
         params <-
           %{}
           |> maybe_put("identity", identity)
           |> maybe_put("label", label)
           |> maybe_put("ttl_seconds", ttl)
           |> maybe_put("scopes", if(scopes == [], do: nil, else: scopes)),
         {:ok, issued} <-
           Client.request(opts[:socket_path], "token.issue", params, token: admin_token) do
      IO.puts("Token: #{issued["token"]}")

      case issued["identity"] do
        %{"name" => name} -> IO.puts("Identity: #{name}")
        _ -> :ok
      end

      IO.puts("Scopes: #{Enum.join(issued["scopes"] || [], ", ")}")

      if is_binary(issued["expires_at"]) do
        IO.puts("Expires: #{issued["expires_at"]}")
      end
    else
      {:error, {"unauthorized", message}} ->
        error(message)

      {:error, {"token_expired", message}} ->
        error(message)

      {:error, {"identity_required", message}} ->
        error(message)

      {:error, {"identity_not_found", message}} ->
        error(message)

      {:error, {"identity_ambiguous", message}} ->
        error(message)

      {:error, {"invalid_scope", message}} ->
        error(message)

      {:error, :enoent} ->
        error("host is not running")

      {:error, _reason} ->
        error("failed to issue host token")
    end
  end

  defp dispatch(_, _opts) do
    IO.puts("""
    usage: arc host <start|status|stop|token issue> [options]

      arc host start [--socket PATH] [--relay host:port] [--relay-pubkey <key>]
      arc host status [--socket PATH]
      arc host stop [--socket PATH]
      arc host token issue --identity <key-or-name> [--scope <scope>]... [--ttl <seconds>] [--label <label>] [--socket PATH]
    """)

    System.halt(1)
  end

  defp relay_address(nil), do: Arc.Net.relay_address()
  defp relay_address(addr), do: Arc.Net.relay_address_from(addr)

  defp resolve_relay_pubkey_pin(nil), do: Arc.Net.relay_pubkey()

  defp resolve_relay_pubkey_pin(value) do
    case Arc.Net.relay_pubkey_from(value) do
      nil -> error("invalid --relay-pubkey (expected 32-byte hex or base64)")
      pubkey -> pubkey
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

  defp pop_multi_opt(args, flag), do: pop_multi_opt(args, flag, [], [])

  defp pop_multi_opt([flag, value | rest], flag, acc, values) do
    pop_multi_opt(rest, flag, acc, [value | values])
  end

  defp pop_multi_opt([h | rest], flag, acc, values) do
    pop_multi_opt(rest, flag, [h | acc], values)
  end

  defp pop_multi_opt([], _flag, acc, values), do: {Enum.reverse(values), Enum.reverse(acc)}

  defp maybe_put(map, _key, nil), do: map
  defp maybe_put(map, _key, []), do: map
  defp maybe_put(map, key, value), do: Map.put(map, key, value)

  @spec error(String.t()) :: no_return()
  defp error(msg) do
    IO.puts(:stderr, "error: #{msg}")
    System.halt(1)
  end
end
