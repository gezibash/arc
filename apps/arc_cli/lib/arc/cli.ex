defmodule Arc.CLI do
  @moduledoc """
  ARC command-line interface.

  Usage: arc <command> [subcommand] [options]
  """

  # Subcommands whose module receives only the remaining args.
  @bare_commands %{
    "keys" => Arc.CLI.Keys,
    "apps" => Arc.CLI.Apps,
    "host" => Arc.CLI.Host,
    "relay" => Arc.CLI.Relay,
    "mcp" => Arc.CLI.MCP
  }

  # Subcommands whose module receives the command name as the first arg.
  @prefixed_commands %{
    "publish" => Arc.CLI.Control,
    "resolve" => Arc.CLI.Control,
    "install" => Arc.CLI.Tools,
    "tool" => Arc.CLI.Tools,
    "trust" => Arc.CLI.Tools,
    "discover" => Arc.CLI.Agent,
    "mount" => Arc.CLI.Agent,
    "send" => Arc.CLI.Agent,
    "info" => Arc.CLI.Agent,
    "listen" => Arc.CLI.Agent,
    "serve" => Arc.CLI.Agent
  }

  # The build embeds the umbrella version and the git commit it was built
  # from. HEAD is an external resource so a new commit triggers a rebuild.
  @external_resource Path.expand("../../../../.git/HEAD", __DIR__)
  @version Mix.Project.config()[:version]
  @git_sha (case System.cmd("git", ["rev-parse", "--short", "HEAD"], stderr_to_stdout: true) do
              {sha, 0} -> String.trim(sha)
              _ -> "unknown"
            end)

  def version_string, do: "arc #{@version} (#{@git_sha})"

  def main(args \\ []) do
    ensure_started()
    {max_frame_bytes, args} = pop_opt(args, "--max-frame-bytes")
    configure_frame_cap(max_frame_bytes)
    dispatch(args)
  end

  defp pop_opt(args, flag), do: pop_opt(args, flag, [])

  defp pop_opt([flag, value | rest], flag, acc), do: {value, Enum.reverse(acc) ++ rest}
  defp pop_opt([h | rest], flag, acc), do: pop_opt(rest, flag, [h | acc])
  defp pop_opt([], _flag, acc), do: {nil, Enum.reverse(acc)}

  # Applies to every relay connection this process opens or accepts:
  # `arc relay`, `arc host start`, `arc mcp`, and the agent commands.
  defp configure_frame_cap(value) do
    case Arc.Net.configure_frame_cap(value) do
      {:error, :invalid} ->
        error(
          "invalid --max-frame-bytes / ARC_RELAY_MAX_FRAME_BYTES value: " <>
            "use a byte count, 0, or unbounded"
        )

      _ ->
        :ok
    end
  end

  defp dispatch([]), do: help()
  defp dispatch(["help"]), do: help()
  defp dispatch(["version"]), do: IO.puts(version_string())
  defp dispatch(["--version"]), do: IO.puts(version_string())

  defp dispatch([command | rest]) do
    cond do
      module = Map.get(@bare_commands, command) ->
        module.run(rest)

      module = Map.get(@prefixed_commands, command) ->
        module.run([command | rest])

      Arc.CLI.Tools.maybe_run_installed(command, rest) ->
        :ok

      true ->
        error("unknown command: #{Enum.join([command | rest], " ")}")
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
                                 (--trust skips the signer prompt for scripts)
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
      version                     Print the arc version and build commit

    Options:
      --relay host:port           Connect to a relay node (for send/listen/serve)
      --relay-pubkey <key>        Pin relay pubkey (hex/base64) for send/listen/serve
      --key <name>                Relay identity key for `arc relay` (or ARC_RELAY_KEY)
      --max-frame-bytes <n>       Largest relay frame this node accepts, in bytes
                                 (0 or unbounded = no cap; default: unbounded)

    Environment:
      ARC_KEY=<name>              Override active key for this terminal
      ARC_RELAY=host:port         Default relay address
      ARC_RELAY_PUBKEY=<key>      Relay pubkey pin (hex/base64) for relay connections
      ARC_RELAY_PORT=PORT         Port for arc relay (default: 7331)
      ARC_RELAY_KEY=<name>        Relay identity key for `arc relay`
      ARC_RELAY_MAX_FRAME_BYTES=<n>
                                 Default for --max-frame-bytes

    Notes:
      Installed tools dispatch as native subcommands: `arc <tool-name> ...`
      Provider bundles use `Arcfile` + `manifest.json`, e.g. `arc serve ~/.arc/providers/sandbox`
      Low-level escape hatch: `exec:///path/to/runtime?manifest=/abs/path/to/manifest.(json|toml)`
    """)
  end

  @spec error(String.t()) :: no_return()
  defp error(msg) do
    IO.puts(:stderr, "error: #{msg}")
    IO.puts(:stderr, "Run 'arc help' for usage.")
    System.halt(1)
  end
end
