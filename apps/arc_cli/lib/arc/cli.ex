defmodule Arc.CLI do
  @moduledoc """
  ARC command-line interface.

  Usage: arc <command> [subcommand] [options]
  """

  def main(args \\ []) do
    ensure_started()

    case args do
      ["keys" | rest] ->
        Arc.CLI.Keys.run(rest)

      ["publish" | rest] ->
        Arc.CLI.Control.run(["publish" | rest])

      ["resolve" | rest] ->
        Arc.CLI.Control.run(["resolve" | rest])

      ["install" | rest] ->
        Arc.CLI.Tools.run(["install" | rest])

      ["apps" | rest] ->
        Arc.CLI.Apps.run(rest)

      ["host" | rest] ->
        Arc.CLI.Host.run(rest)

      ["tool" | rest] ->
        Arc.CLI.Tools.run(["tool" | rest])

      ["trust" | rest] ->
        Arc.CLI.Tools.run(["trust" | rest])

      ["relay" | rest] ->
        Arc.CLI.Relay.run(rest)

      ["mcp" | rest] ->
        Arc.CLI.MCP.run(rest)

      ["discover" | rest] ->
        Arc.CLI.Agent.run(["discover" | rest])

      ["mount" | rest] ->
        Arc.CLI.Agent.run(["mount" | rest])

      ["send" | rest] ->
        Arc.CLI.Agent.run(["send" | rest])

      ["info" | rest] ->
        Arc.CLI.Agent.run(["info" | rest])

      ["listen" | rest] ->
        Arc.CLI.Agent.run(["listen" | rest])

      ["serve" | rest] ->
        Arc.CLI.Agent.run(["serve" | rest])

      ["help"] ->
        help()

      [] ->
        help()

      [command | rest] ->
        if Arc.CLI.Tools.maybe_run_installed(command, rest) do
          :ok
        else
          error("unknown command: #{Enum.join([command | rest], " ")}")
        end
    end
  end

  defp ensure_started do
    Application.ensure_all_started(:arc_data)
    Application.ensure_all_started(:arc_mcp)
    Application.ensure_all_started(:arc_net)
  end

  defp help do
    IO.puts("""
    arc — a network for agents

    Usage: arc <command> [subcommand] [options]

    Commands:
      keys gen                    Generate a new key
      keys ls                     List all keys (* marks active)
      keys use <name>             Set the active key
      keys show                   Show the resolved active key
      keys rm <name>              Remove a key
      publish                     Publish current identity to the control plane
      resolve <query>             Resolve a name, petname, or public key prefix
      install <peer> <id>         Install a remote ARC capability as a local command
      apps init [path]            Scaffold a local ARC provider bundle (Arcfile + manifest.json)
      host <...>                  Run the local ARC host service and issue delegated SDK tokens
      tool <subcommand>           Manage installed ARC tools
      trust <subcommand>          Manage trusted remote signers
      discover [query]            Search remote capability summaries
      mount <task> ...            Manage task-scoped mounted capabilities
      mcp <task>                  Serve mounted capabilities as MCP tools over local Streamable HTTP
      send <to> <message>         Send a message (waits for reply)
      info <peer> [capability]    Fetch remote capability summary or one detail view
      listen                      Listen for incoming messages
      serve <target>              Serve a provider bundle or runtime URI with live request logs
      relay [--port PORT] [--key NAME]
                                 Run a relay node (routes encrypted packets by pubkey)

    Options:
      --relay host:port           Connect to a relay node (for send/listen/serve)
      --relay-pubkey <key>        Pin relay pubkey (hex/base64) for send/listen/serve
      --key <name>                Relay identity key for `arc relay` (or ARC_RELAY_KEY)

    Environment:
      ARC_KEY=<name>              Override active key for this terminal
      ARC_RELAY=host:port         Default relay address
      ARC_RELAY_PUBKEY=<key>      Relay pubkey pin (hex/base64) for relay connections
      ARC_RELAY_PORT=PORT         Port for arc relay (default: 7331)
      ARC_RELAY_KEY=<name>        Relay identity key for `arc relay`

    Notes:
      Installed tools dispatch as native subcommands: `arc <tool-name> ...`
      Provider bundles use `Arcfile` + `manifest.json`, e.g. `arc serve ~/.arc/providers/sandbox`
      Low-level escape hatch: `exec:///path/to/runtime?manifest=/abs/path/to/manifest.(json|toml)`
    """)
  end

  defp error(msg) do
    IO.puts(:stderr, "error: #{msg}")
    IO.puts(:stderr, "Run 'arc help' for usage.")
    System.halt(1)
  end
end
