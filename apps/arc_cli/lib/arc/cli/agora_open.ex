defmodule Arc.CLI.AgoraOpen do
  @moduledoc """
  Opens an installed Agora board using the active local citizen identity.
  """

  alias Arc.CLI.AgoraWeb
  alias Arc.CLI.ToolRegistry
  alias Arc.Data.Agora
  alias Arc.Identity.KeyStore

  @usage "usage: arc apps open <installed-command> [--port PORT] [--relay host:port] [--relay-pubkey KEY]"

  def run(args) do
    case start(args) do
      {:ok, server} ->
        try do
          case open_browser(AgoraWeb.launch_url(server)) do
            :ok ->
              IO.puts("Agora opened at #{AgoraWeb.url(server)}")
              IO.puts("Keep this process running. Press Ctrl+C to close the local interface.")
              Process.sleep(:infinity)

            :error ->
              error("could not open a browser; configure your default browser and run again")
          end
        after
          if Process.alive?(server), do: GenServer.stop(server, :normal)
        end

      {:error, message} ->
        error(message)
    end
  end

  @doc false
  def start(args) do
    with {:ok, command, opts} <- parse_options(args),
         {:ok, identity} <- active_identity(),
         {:ok, tool} <- installed_agora(identity, command) do
      case AgoraWeb.start_link([identity: identity, tool: tool] ++ opts) do
        {:ok, server} ->
          {:ok, server}

        {:error, _reason} ->
          {:error, "could not start Agora; check the local port and relay connection"}
      end
    end
  end

  defp parse_options(args) do
    {opts, positional, invalid} =
      OptionParser.parse(args, strict: [port: :integer, relay: :string, relay_pubkey: :string])

    case {positional, invalid} do
      {[command], []} when command != "" -> validate_options(command, opts)
      _ -> {:error, @usage}
    end
  end

  defp validate_options(command, opts) do
    port = Keyword.get(opts, :port, 0)
    relay_text = Keyword.get(opts, :relay, System.get_env("ARC_RELAY"))
    pin_text = Keyword.get(opts, :relay_pubkey, System.get_env("ARC_RELAY_PUBKEY"))
    relay = if relay_text, do: Arc.Net.relay_address_from(relay_text)
    pin = if pin_text, do: Arc.Net.relay_pubkey_from(pin_text)

    cond do
      port < 0 or port > 65_535 ->
        {:error, "invalid --port (expected 0 through 65535)"}

      relay_text != nil and relay == nil ->
        {:error, "invalid relay address (expected host:port)"}

      pin_text != nil and pin == nil ->
        {:error, "invalid relay public key (expected 32-byte hex or base64)"}

      pin != nil and relay == nil ->
        {:error, "a relay public-key pin requires a relay address"}

      true ->
        {:ok, command, [port: port, relay: relay, relay_pubkey: pin]}
    end
  end

  defp active_identity do
    case KeyStore.resolve_active() do
      {:ok, identity} ->
        {:ok, identity}

      {:error, :no_default} ->
        {:error, "no active identity; run 'arc keys gen' or 'arc keys use <name>'"}

      {:error, :ambiguous} ->
        {:error, "ARC_KEY matches multiple identities; use a full key name"}

      {:error, _reason} ->
        {:error, "could not load the active ARC identity"}
    end
  end

  defp installed_agora(identity, command) do
    case ToolRegistry.get(identity, command) do
      {:ok, tool} ->
        if Agora.enabled?(tool["capability"]) do
          {:ok, tool}
        else
          {:error, "the installed command does not expose an Agora interface"}
        end

      {:error, :not_found} ->
        {:error,
         "this command is not installed for the active identity; install an Agora board first"}

      {:error, _reason} ->
        {:error, "could not read the active identity's installed tools"}
    end
  end

  defp open_browser(url) do
    command = if :os.type() == {:unix, :darwin}, do: "open", else: "xdg-open"

    with executable when is_binary(executable) <- System.find_executable(command),
         {_output, 0} <- System.cmd(executable, [url], stderr_to_stdout: true) do
      :ok
    else
      _ -> :error
    end
  rescue
    _ -> :error
  end

  defp error(message) do
    IO.puts(:stderr, "error: #{message}")
    Arc.CLI.Exit.halt(1)
  end
end
