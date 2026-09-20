defmodule Arc.CLI do
  @moduledoc """
  ARC command-line interface.

  Usage: arc <command> [subcommand] [options]
  """

  # Subcommands whose module receives only the remaining args.
  @bare_commands %{
    "keys" => Arc.CLI.Keys,
    "join" => Arc.CLI.Join,
    "apps" => Arc.CLI.Apps,
    "host" => Arc.CLI.Host,
    "status" => Arc.CLI.Status,
    "update" => Arc.CLI.Update,
    "service" => Arc.CLI.Update.Service,
    "relay" => Arc.CLI.Relay,
    "lists" => Arc.CLI.Lists,
    "cache" => Arc.CLI.Cache,
    "request" => Arc.CLI.ProtocolRequest
  }

  # Subcommands whose module receives the command name as the first arg.
  @prefixed_commands %{
    "publish" => Arc.CLI.Control,
    "resolve" => Arc.CLI.Control,
    "install" => Arc.CLI.Tools,
    "tool" => Arc.CLI.Tools,
    "trust" => Arc.CLI.Tools,
    "discover" => Arc.CLI.Agent,
    "send" => Arc.CLI.Agent,
    "info" => Arc.CLI.Agent,
    "listen" => Arc.CLI.Agent,
    "serve" => Arc.CLI.Agent
  }

  # The build embeds the umbrella version and the git commit it was built
  # from. HEAD is an external resource so a new commit triggers a rebuild.
  @git_dir Path.expand("../../../../.git", __DIR__)
  @external_resource Path.join(@git_dir, "HEAD")
  # HEAD usually points at a branch ref. Track that file too, so a new
  # commit on the branch triggers a rebuild.
  @external_resource (case File.read(Path.join(@git_dir, "HEAD")) do
                        {:ok, "ref: " <> ref} -> Path.join(@git_dir, String.trim(ref))
                        _ -> Path.join(@git_dir, "HEAD")
                      end)
  @version Mix.Project.config()[:version]
  @git_sha (case System.cmd("git", ["rev-parse", "--short", "HEAD"], stderr_to_stdout: true) do
              {sha, 0} -> String.trim(sha)
              _ -> "unknown"
            end)

  def version_string, do: "arc #{@version} (#{@git_sha})"

  def main(args \\ []) do
    configure_stdio()
    ensure_started()
    {max_frame_bytes, args} = pop_opt(args, "--max-frame-bytes")
    configure_frame_cap(max_frame_bytes)
    dispatch(args)
  rescue
    e in Arc.CLI.Exit.Error -> Arc.CLI.Exit.finish(e.code)
  end

  defp pop_opt(args, flag), do: pop_opt(args, flag, [])

  defp pop_opt([flag, value | rest], flag, acc), do: {value, Enum.reverse(acc) ++ rest}
  defp pop_opt([h | rest], flag, acc), do: pop_opt(rest, flag, [h | acc])
  defp pop_opt([], _flag, acc), do: {nil, Enum.reverse(acc)}

  # Applies to every relay connection this process opens or accepts:
  # `arc relay`, `arc host start`, and the agent commands.
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

  # An escript starts with latin1 stdio. `IO.read` on a latin1 device
  # re-encodes every UTF-8 byte as a code point, and `IO.write` strips a
  # layer on the way out, so a non-ASCII page body arrives at the provider
  # double-encoded and prints back as mojibake. Unicode stdio passes valid
  # UTF-8 through unchanged in both directions.
  defp configure_stdio do
    :io.setopts(:standard_io, encoding: :unicode)
    :io.setopts(:standard_error, encoding: :unicode)
  end

  defp ensure_started do
    Application.ensure_all_started(:arc_data)
    Application.ensure_all_started(:arc_net)
  end

  defp help do
    IO.puts("""
    arc — a network for agents

    Usage: arc <command> [subcommand] [options]

    Commands:
      join <host[:port]>          Join a relay and remember it as your default
      keys gen                    Generate a new key
      keys ls                     List all keys (* marks active)
      keys use <name>             Set the global default key
      keys show                   Show the resolved active key
      keys rm <name>              Remove a key
      publish                     Publish current identity to the control plane
      resolve <query>             Resolve a name, petname, or public key prefix
      install <peer> <id>         Install a remote ARC capability as a local command
                                 (--trust skips the signer prompt for scripts)
      apps init [path]            Scaffold a local ARC provider bundle (Arcfile + manifest.json)
      apps open <command>        Open an installed Agora board in your local browser
      host <...>                  Run the local ARC host service and issue delegated SDK tokens
      status [--format json]      Query the configured relay’s status
      update [check|status]       Install the newest release published on your relay
                                 (--socket PATH controls a managed relay service instead)
      service start --config PATH Start an upgrade-capable relay (native release)
      tool <subcommand>           Manage installed ARC tools
      trust <subcommand>          Manage trusted remote signers
      discover [query]            Search remote capability summaries
                                 (--limit N and --after CURSOR page relay results)
      send <to> <message>         Send a message (waits for reply)
      request <uri> ...          Send an opaque body to a scheme+arc:// service
      info <peer> [capability]    Fetch remote capability summary or one detail view
      listen                      Listen for incoming messages
      serve <target>              Serve a provider bundle or runtime URI with live request logs
                                 (--federate shares this live announcement with direct relay partners)
                                 (--federate-network permits onward relay federation)
      relay [--port PORT] [--key NAME] [--peer PUBKEY@HOST:PORT]...
                                 Run a relay node (routes encrypted packets by pubkey)
      lists add|rm|ls <tool> ...  Saved peer lists a tool's commands expand
      cache on|off|search <tool>  Local sealed cache of opened records, and search
      version                     Print the arc version and build commit

    Options:
      --relay host:port           Use a relay for discovery and peer commands
      --relay-pubkey <key>        Pin the relay public key (hex/base64)
      --federate                  Permit `serve` or `listen` discovery through direct federation
                                 (requires a configured relay and relay public-key pin)
      --federate-network          Permit onward federation from `serve` or `listen`
                                 (requires a configured relay and relay public-key pin)
      --direct-policy PATH        Permit only listed direct connection scopes for `serve` or `request`
                                 (requires a configured relay and relay public-key pin; peers learn listed addresses)
      --key <name>                Relay identity key for `arc relay` (or ARC_RELAY_KEY)
      --peer <key>@host:port      Approve a direct federation peer; repeat for each partner
      --transit                   Allow this relay to forward approved federation traffic
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
      Identity: ARC_KEY -> ./arc.key -> ~/.config/arc/default.key (key names, current folder only)
      Installed tools dispatch as native subcommands: `arc <tool-name> ...`
      Provider bundles use `Arcfile` + `manifest.json`, e.g. `arc serve ~/.arc/providers/sandbox`
      Low-level escape hatch: `exec:///path/to/runtime?manifest=/abs/path/to/manifest.(json|toml)`
    """)
  end

  @spec error(String.t()) :: no_return()
  defp error(msg) do
    IO.puts(:stderr, "error: #{msg}")
    IO.puts(:stderr, "Run 'arc help' for usage.")
    Arc.CLI.Exit.halt(1)
  end
end
