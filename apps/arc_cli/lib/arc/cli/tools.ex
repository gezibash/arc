defmodule Arc.CLI.Tools do
  @moduledoc """
  Durable ARC tool installation and invocation commands.
  """

  alias Arc.CLI.ToolRegistry
  alias Arc.CLI.TrustStore
  alias Arc.Data.Agent
  alias Arc.Data.CapabilityDiscovery
  alias Arc.Data.CapabilityInvocation
  alias Arc.Data.CapabilityPackage
  alias Arc.Data.InterfaceManifest
  alias Arc.Data.Toolbox
  alias Arc.Identity
  alias Arc.Identity.KeyStore

  def run(args) do
    {command_name, args} = pop_opt(args, "--as")
    {relay_pubkey, args} = pop_opt(args, "--relay-pubkey")
    {relay_addr, args} = pop_opt(args, "--relay")
    {trust, args} = pop_flag(args, "--trust")
    {yes, clean_args} = pop_flag(args, "--yes")

    opts = [
      command: command_name,
      relay: relay_addr,
      relay_pubkey: relay_pubkey,
      trust: trust or yes
    ]

    dispatch(clean_args, opts)
  end

  @doc """
  Maps a trust prompt answer to a decision.

  `:allow` for `y`/`yes`, `:deny` for `n`/`no`, `:cancel` for an empty answer,
  any other text, or EOF. Only `:deny` records a trust decision.
  """
  @spec trust_answer(String.t() | :eof | {:error, term()}) :: :allow | :deny | :cancel
  def trust_answer(answer) when is_binary(answer) do
    case answer |> String.trim() |> String.downcase() do
      a when a in ["y", "yes"] -> :allow
      a when a in ["n", "no"] -> :deny
      _ -> :cancel
    end
  end

  def trust_answer(_eof_or_error), do: :cancel

  def maybe_run_installed(command, args) when is_binary(command) do
    {relay_pubkey, args} = pop_opt(args, "--relay-pubkey")
    {relay_addr, clean_args} = pop_opt(args, "--relay")
    opts = [relay: relay_addr, relay_pubkey: relay_pubkey]

    case active_identity() do
      {:ok, id} ->
        if ToolRegistry.installed?(id, command) do
          invoke_command(command, clean_args, opts)
          true
        else
          false
        end

      _ ->
        false
    end
  end

  defp dispatch(["install", peer, capability_id | _], opts) do
    with_agent(
      fn agent, id ->
        case CapabilityDiscovery.fetch_detail(agent, peer, capability_id) do
          {:ok, detail} ->
            with {:ok, verified} <- CapabilityPackage.verify(detail),
                 {:ok, trust_state} <- ensure_trusted(id, verified, opts),
                 {:ok, install} <-
                   ToolRegistry.install(
                     id,
                     verified,
                     command: opts[:command],
                     trust_state_at_install: trust_state
                   ) do
              print_install(install)
            else
              {:error, :reserved_command} ->
                error("install failed: '#{opts[:command]}' is reserved by the built-in CLI")

              {:error, :invalid_command} ->
                error("install failed: invalid command name")

              {:error, :not_installable} ->
                error("install failed: capability does not publish an installable interface")

              {:error, :command_conflict} ->
                error("install failed: command already installed")

              {:error, :signer_conflict} ->
                error("install failed: command is already bound to a different signer")

              {:error, {:signer_denied, signer}} ->
                error(
                  "install failed: signer is denied by local trust policy. " <>
                    "To allow it, run: arc trust allow #{signer}"
                )

              {:error, {:trust_denied, signer}} ->
                error(
                  "install cancelled: signer denied. " <>
                    "To undo, run: arc trust allow #{signer}"
                )

              {:error, :trust_declined} ->
                error(
                  "install cancelled: signer not trusted. " <>
                    "Answer 'y' at the prompt or pass --trust to skip it"
                )

              {:error, reason} ->
                error("install failed: #{inspect(reason)}")
            end

          {:error, {:remote, code, message}} ->
            error("install failed: #{code}: #{message}")

          {:error, reason} ->
            error("install failed: #{inspect(reason)}")
        end
      end,
      opts
    )
  end

  defp dispatch(["tool", "ls" | _], _opts) do
    with_identity(fn id ->
      case ToolRegistry.list(id) do
        {:ok, tools} -> print_tools(id, tools)
        {:error, reason} -> error("tool ls failed: #{inspect(reason)}")
      end
    end)
  end

  defp dispatch(["tool", "info", command | _], _opts) do
    with_identity(fn id ->
      case ToolRegistry.get(id, command) do
        {:ok, tool} -> print_tool_info(tool)
        {:error, :not_found} -> error("tool info failed: '#{command}' is not installed")
        {:error, reason} -> error("tool info failed: #{inspect(reason)}")
      end
    end)
  end

  defp dispatch(["tool", "verify", command | _], _opts) do
    with_identity(fn id ->
      case ToolRegistry.get(id, command) do
        {:ok, tool} ->
          case CapabilityPackage.verify(ToolRegistry.to_signed_package(tool)) do
            {:ok, _verified} ->
              IO.puts("Verified #{command}")
              IO.puts("  Signer: #{tool["signer_public_key"]}")
              IO.puts("  Hash: #{tool["package_hash"]}")

            {:error, reason} ->
              error("tool verify failed: #{inspect(reason)}")
          end

        {:error, :not_found} ->
          error("tool verify failed: '#{command}' is not installed")

        {:error, reason} ->
          error("tool verify failed: #{inspect(reason)}")
      end
    end)
  end

  defp dispatch(["tool", "diff", command | _], opts) do
    with_agent(
      fn agent, id ->
        with {:ok, tool} <- ToolRegistry.get(id, command),
             {:ok, remote} <- fetch_remote_package(agent, tool),
             :ok <- ensure_same_signer(tool, remote) do
          print_diff(tool, remote)
        else
          {:error, :not_found} ->
            error("tool diff failed: '#{command}' is not installed")

          {:error, :signer_changed} ->
            error("tool diff failed: remote signer changed")

          {:error, {:remote, code, message}} ->
            error("tool diff failed: #{code}: #{message}")

          {:error, reason} ->
            error("tool diff failed: #{inspect(reason)}")
        end
      end,
      opts
    )
  end

  defp dispatch(["tool", "update", command | _], opts) do
    with_agent(
      fn agent, id ->
        with {:ok, tool} <- ToolRegistry.get(id, command),
             :ok <- ensure_not_pinned(tool),
             {:ok, remote} <- fetch_remote_package(agent, tool),
             :ok <- ensure_same_signer(tool, remote) do
          if tool["package_hash"] == remote["package_hash"] do
            IO.puts("#{command} is already up to date")
          else
            update_installed_tool(id, tool, remote)
          end
        else
          {:error, :not_found} ->
            error("tool update failed: '#{command}' is not installed")

          {:error, :pinned} ->
            error(
              "tool update failed: '#{command}' is pinned; run 'arc tool unpin #{command}' first"
            )

          {:error, :signer_changed} ->
            error("tool update failed: remote signer changed")

          {:error, {:remote, code, message}} ->
            error("tool update failed: #{code}: #{message}")

          {:error, reason} ->
            error("tool update failed: #{inspect(reason)}")
        end
      end,
      opts
    )
  end

  defp dispatch(["tool", "call", command | input_parts], opts) do
    invoke_command(command, input_parts, opts)
  end

  defp dispatch(["tool", "pin", command | _], _opts) do
    with_identity(fn id ->
      case ToolRegistry.set_pinned(id, command, true) do
        {:ok, _tool} -> IO.puts("Pinned #{command}")
        {:error, :not_found} -> error("tool pin failed: '#{command}' is not installed")
        {:error, reason} -> error("tool pin failed: #{inspect(reason)}")
      end
    end)
  end

  defp dispatch(["tool", "unpin", command | _], _opts) do
    with_identity(fn id ->
      case ToolRegistry.set_pinned(id, command, false) do
        {:ok, _tool} -> IO.puts("Unpinned #{command}")
        {:error, :not_found} -> error("tool unpin failed: '#{command}' is not installed")
        {:error, reason} -> error("tool unpin failed: #{inspect(reason)}")
      end
    end)
  end

  defp dispatch(["tool", "rm", command | _], _opts) do
    with_identity(fn id ->
      case ToolRegistry.uninstall(id, command) do
        :ok ->
          IO.puts("Removed tool #{command}")

        {:error, reason} ->
          error("tool rm failed: #{inspect(reason)}")
      end
    end)
  end

  defp dispatch(["trust", "ls" | _], _opts) do
    with_identity(fn id ->
      case TrustStore.list(id) do
        {:ok, signers} -> print_trust(id, signers)
        {:error, reason} -> error("trust ls failed: #{inspect(reason)}")
      end
    end)
  end

  defp dispatch(["trust", "allow", signer | _], _opts) do
    with_identity(fn id ->
      case TrustStore.allow(id, signer) do
        {:ok, _record} -> IO.puts("Allowed signer #{String.downcase(signer)}")
        {:error, reason} -> error("trust allow failed: #{inspect(reason)}")
      end
    end)
  end

  defp dispatch(["trust", "deny", signer | _], _opts) do
    with_identity(fn id ->
      case TrustStore.deny(id, signer) do
        {:ok, _record} -> IO.puts("Denied signer #{String.downcase(signer)}")
        {:error, reason} -> error("trust deny failed: #{inspect(reason)}")
      end
    end)
  end

  defp dispatch(_, _opts) do
    IO.puts("""
    arc tool commands

    Commands:
      install <peer> <id> [--as NAME]
      tool ls
      tool info <name>
      tool verify <name>
      tool diff <name>
      tool update <name>
      tool call <name> [subcommand] [args]
      tool pin <name>
      tool unpin <name>
      tool rm <name>
      trust ls
      trust allow <signer-public-key>
      trust deny <signer-public-key>

    Options:
      --as NAME               Local command namespace for the installed capability
      --relay host:port       Connect through a relay node
      --relay-pubkey <key>    Pin relay identity pubkey (hex/base64)
    """)
  end

  defp update_installed_tool(id, tool, remote) do
    print_diff(tool, remote)

    case ToolRegistry.install(
           id,
           remote,
           command: tool["command"],
           trust_state_at_install: tool["trust_state_at_install"] || "allowed",
           pinned: false
         ) do
      {:ok, install} ->
        IO.puts("")
        print_install(install)

      {:error, reason} ->
        error("tool update failed: #{inspect(reason)}")
    end
  end

  defp invoke_command(command, input_parts, opts) do
    with_agent(
      fn agent, id ->
        case ToolRegistry.get(id, command) do
          {:ok, install} ->
            invoke_installed(agent, command, install, input_parts)

          {:error, :not_found} ->
            error("tool call failed: '#{command}' is not installed for #{Identity.name(id)}")

          {:error, reason} ->
            error("tool call failed: #{inspect(reason)}")
        end
      end,
      opts
    )
  end

  defp invoke_installed(agent, command, install, input_parts) do
    {help?, clean_args} = extract_help(input_parts)
    {output_opts, clean_args} = extract_output_flags(clean_args)

    if help? do
      print_usage(command, install, clean_args)
    else
      case build_invocation(install, clean_args) do
        {:ok, invocation} ->
          invoke_built_command(agent, install, Map.put(invocation, :output_opts, output_opts))

        {:error, {:invalid_arguments, message}} ->
          error("tool call failed: #{message}")
      end
    end
  end

  defp print_install(install) do
    provider = install["provider"] || %{}
    capability = install["capability"] || %{}
    provider_name = provider["name"] || provider["short_name"] || "unknown"
    capability_id = capability["id"] || "unknown"
    kind = capability["kind"] || "capability"
    scheme = capability["scheme"] || "unknown"
    command = install["command"] || "tool"
    usage = install["usage"] || ToolRegistry.usage_from_capability(command, capability)

    IO.puts("Installed #{command} from #{provider_name}/#{capability_id} [#{kind}/#{scheme}]")
    IO.puts("Version: #{install["release_version"]} (#{install["channel"]})")
    IO.puts("Signer: #{short_key(install["signer_public_key"])}")
    IO.puts("Hash: #{short_hash(install["package_hash"])}")
    IO.puts("Run: #{usage}")
  end

  defp print_tools(owner, tools) do
    IO.puts("Owner: #{Identity.name(owner)}")
    IO.puts("Installed: #{length(tools)}")

    Enum.each(tools, fn tool ->
      capability = tool["capability"] || %{}

      IO.puts("")
      print_tool_heading(tool, capability)
      print_tool_commands(capability)
      print_tool_meta(tool, capability["invocation"] || %{})
    end)
  end

  defp print_tool_heading(tool, capability) do
    provider = tool["provider"] || %{}
    provider_name = provider["name"] || provider["short_name"] || "unknown"
    capability_id = capability["id"] || "unknown"
    kind = capability["kind"] || "capability"
    scheme = capability["scheme"] || "unknown"
    usage = tool["usage"] || ToolRegistry.usage_from_capability(tool["command"], capability)
    summary = ToolRegistry.command_summary(tool["command"], capability)

    IO.puts("#{tool["command"]} -> #{provider_name}/#{capability_id} [#{kind}/#{scheme}]")
    IO.puts("  Usage: #{usage}")

    if summary != "" do
      IO.puts("  Summary: #{summary}")
    end
  end

  defp print_tool_commands(capability) do
    if ToolRegistry.namespace_required?(capability) do
      labels =
        capability
        |> ToolRegistry.subcommands()
        |> Enum.map_join(", ", &ToolRegistry.command_label/1)

      IO.puts("  Commands: #{labels}")
    end
  end

  defp print_tool_meta(tool, invocation) do
    IO.puts("  Invocation: #{invocation["method"] || "RAW"} #{invocation["path"] || "/"}")
    IO.puts("  Release: #{tool["release_version"]} (#{tool["channel"]})")
    IO.puts("  Signer: #{short_key(tool["signer_public_key"])}")
    IO.puts("  Hash: #{short_hash(tool["package_hash"])}")
    IO.puts("  Pinned: #{if(tool["pinned"], do: "yes", else: "no")}")
    IO.puts("  Mode: #{invocation["mode"] || "request_reply"}")
  end

  defp print_tool_info(tool) do
    provider = tool["provider"] || %{}
    capability = tool["capability"] || %{}
    provider_name = provider["name"] || provider["short_name"] || "unknown"

    IO.puts("Command: #{tool["command"]}")
    IO.puts("Provider: #{provider_name}")
    IO.puts("Capability: #{tool["capability_id"]}")
    IO.puts("Type: #{capability["kind"]}/#{capability["scheme"]}")
    IO.puts("Version: #{tool["release_version"]}")
    IO.puts("Channel: #{tool["channel"]}")
    IO.puts("Pinned: #{if(tool["pinned"], do: "yes", else: "no")}")
    IO.puts("Mode: #{get_in(capability, ["invocation", "mode"]) || "request_reply"}")
    IO.puts("Signer: #{tool["signer_public_key"]}")
    IO.puts("Hash: #{tool["package_hash"]}")

    if is_binary(tool["published_at"]) do
      IO.puts("Published: #{tool["published_at"]}")
    end

    IO.puts("Installed at: #{tool["installed_at"]}")
    IO.puts("Trust at install: #{tool["trust_state_at_install"]}")
    IO.puts("Usage: #{tool["usage"]}")
  end

  defp print_trust(owner, signers) do
    IO.puts("Owner: #{Identity.name(owner)}")
    IO.puts("Trusted signers: #{length(signers)}")

    Enum.each(signers, fn signer ->
      IO.puts("")
      IO.puts("#{signer["signer_public_key"]}")
      IO.puts("  State: #{signer["state"]}")
      IO.puts("  Scope: #{signer["scope"] || "global"}")
      IO.puts("  First trusted at: #{signer["first_trusted_at"]}")
    end)
  end

  defp print_diff(tool, remote) do
    local = ToolRegistry.to_signed_package(tool)
    changes = CapabilityPackage.diff(local, remote)

    if changes == [] do
      IO.puts("No changes.")
    else
      IO.puts("Changes:")
      Enum.each(changes, fn change -> IO.puts("  #{change}") end)
    end
  end

  # `--raw` keeps only open and petnames, so tab-separated lines print as
  # the provider sent them. `--hex` drops petnames.
  defp apply_output_filter(reply, %{"output" => %{"filters" => filters}}, opts)
       when is_list(filters) do
    %{identity: identity} = filter_context()

    filters =
      Enum.reject(filters, fn filter ->
        (opts[:raw] and filter not in ["open", "petnames"]) or
          (opts[:hex] and filter == "petnames")
      end)

    Map.update(reply, :text, nil, fn
      text when is_binary(text) -> Toolbox.apply_output_filters(text, filters, identity)
      other -> other
    end)
  end

  defp apply_output_filter(reply, _command, _opts), do: reply

  defp extract_output_flags(argv) do
    {flags, rest} = Enum.split_with(argv, &(&1 in ["--raw", "--hex", "--notify", "--once"]))

    {%{
       raw: "--raw" in flags,
       hex: "--hex" in flags,
       notify: "--notify" in flags,
       once: "--once" in flags
     }, rest}
  end

  # An events command sends its request once, as a hello, then prints every
  # event the provider emits to this identity whose topic matches. It runs
  # until Ctrl+C, or after the first event with --once.
  defp invoke_events_command(agent, install, built) do
    invocation = built.invocation
    opts = Map.get(built, :output_opts, %{})
    provider_pk = decode_provider_pk(install)
    topics = invocation["topics"]

    case CapabilityInvocation.invoke(agent, install, built.input, invocation_override: invocation) do
      {:ok, _reply} -> :ok
      {:error, {:remote, code, message}} -> error("tool call failed: #{code}: #{message}")
      {:error, reason} -> error("tool call failed: #{inspect(reason)}")
    end

    IO.puts("watching. Ctrl+C to stop.")
    events_loop(agent, provider_pk, topics, Map.get(built, :command), opts)
  end

  defp events_loop(agent, provider_pk, topics, command, opts) do
    Agent.poll_mailbox(agent)

    events =
      agent
      |> Agent.read_inbox()
      |> Enum.filter(fn msg ->
        msg[:kind] == :event and
          (is_nil(provider_pk) or msg[:from_key] == provider_pk) and
          topic_matches?(get_in(msg, [:meta, "topic"]), topics)
      end)

    Enum.each(events, &print_event(&1, command, opts))

    if opts[:once] and events != [] do
      :ok
    else
      Process.sleep(200)
      events_loop(agent, provider_pk, topics, command, opts)
    end
  end

  defp print_event(event, command, opts) do
    meta = event[:meta] || %{}
    topic = meta["topic"] || "event"
    sender = meta["from"] || Identity.encode_public_key(event[:from_key])
    body = event[:text] || ""
    time = Calendar.strftime(DateTime.utc_now(), "%H:%M:%S")

    text =
      "#{time}  #{topic}  #{sender}  #{body}"
      |> apply_text_filters(command, opts)

    IO.puts(text)
    if opts[:notify], do: desktop_notify(topic, text)
  end

  defp apply_text_filters(text, command, opts) do
    %{text: text}
    |> apply_output_filter(command, opts)
    |> Map.get(:text)
  end

  defp topic_matches?(_topic, nil), do: true
  defp topic_matches?(nil, _globs), do: false

  defp topic_matches?(topic, globs) do
    Enum.any?(globs, fn glob ->
      pattern = "^" <> (glob |> Regex.escape() |> String.replace("\\*", ".*")) <> "$"
      Regex.match?(Regex.compile!(pattern), topic)
    end)
  end

  defp decode_provider_pk(install) do
    case install["provider_public_key"] do
      hex when is_binary(hex) ->
        case Base.decode16(hex, case: :mixed) do
          {:ok, <<pk::binary-size(32)>>} -> pk
          _ -> nil
        end

      _ ->
        nil
    end
  end

  # The body is opened on this machine before it reaches the notifier, and
  # only the first line goes. macOS and Linux only; elsewhere this is a no-op.
  defp desktop_notify(topic, text) do
    first = text |> String.split("\n") |> hd() |> String.slice(0, 200)

    case :os.type() do
      {:unix, :darwin} ->
        script = ~s(display notification "#{escape_quotes(first)}" with title "arc #{topic}")
        System.cmd("osascript", ["-e", script], stderr_to_stdout: true)

      {:unix, _} ->
        System.cmd("notify-send", ["arc #{topic}", first], stderr_to_stdout: true)

      _ ->
        :ok
    end

    :ok
  rescue
    _ -> :ok
  end

  defp escape_quotes(text), do: String.replace(text, "\"", "\\\"")

  defp print_reply(reply) do
    case reply[:kind] do
      :response -> IO.puts(reply.text || "")
      :error -> IO.puts(reply.text || "")
      _ -> IO.puts(reply.text || "")
    end
  end

  defp print_usage(command, install, argv) do
    capability = install["capability"] || %{}

    case select_help_command(capability, argv) do
      {:ok, cli_command} ->
        print_command_usage(command, capability, cli_command)

      :overview ->
        print_namespace_usage(command, capability)
    end
  end

  defp print_namespace_usage(command, capability) do
    summary = ToolRegistry.command_summary(command, capability)
    usage = ToolRegistry.usage_from_capability(command, capability)
    root = ToolRegistry.root_command(capability)
    subcommands = ToolRegistry.subcommands(capability)

    if summary != "" do
      IO.puts(summary)
      IO.puts("")
    end

    IO.puts("Usage: #{usage}")

    if root && subcommands != [] do
      IO.puts("")
      IO.puts("Default:")
      IO.puts("  #{ToolRegistry.command_usage(command, root)}")
    end

    if subcommands != [] do
      IO.puts("")
      IO.puts("Commands:")

      Enum.each(subcommands, fn cli_command ->
        label = ToolRegistry.command_label(cli_command)
        summary = cli_command["summary"] || ""
        IO.puts("  #{label}  #{summary}")
      end)
    end

    if root && subcommands == [] do
      print_argument_sections(root)
      print_examples(command, capability, root)
    end
  end

  defp print_command_usage(namespace, capability, cli_command) do
    summary = cli_command["summary"] || ToolRegistry.command_summary(namespace, capability)
    usage = ToolRegistry.command_usage(namespace, cli_command)

    if summary != "" do
      IO.puts(summary)
      IO.puts("")
    end

    IO.puts("Usage: #{usage}")
    print_argument_sections(cli_command)
    print_examples(namespace, capability, cli_command)
  end

  defp print_argument_sections(cli_command) do
    args = Map.get(cli_command, "args", [])

    case Enum.filter(args, &(&1["kind"] == "positional")) do
      [] ->
        :ok

      positional ->
        IO.puts("")
        IO.puts("Arguments:")

        Enum.each(positional, fn arg ->
          IO.puts("  #{usage_label(arg)}  #{arg["description"] || ""}")
        end)
    end

    case Enum.filter(args, &(&1["kind"] == "option")) do
      [] ->
        :ok

      options ->
        IO.puts("")
        IO.puts("Options:")

        Enum.each(options, fn arg ->
          IO.puts("  #{usage_label(arg)}  #{arg["description"] || ""}")
        end)
    end
  end

  defp print_examples(namespace, capability, cli_command) do
    examples =
      case Map.get(cli_command, "examples") do
        list when is_list(list) and list != [] -> list
        _ -> capability["examples"] || []
      end

    if is_list(examples) and examples != [] do
      IO.puts("")
      IO.puts("Examples:")

      Enum.each(examples, fn example ->
        prefix =
          case Map.get(cli_command, "path", []) do
            [] -> "arc " <> namespace
            path -> "arc " <> namespace <> " " <> Enum.join(path, " ")
          end

        IO.puts("  #{prefix} #{example}" |> String.trim())
      end)
    end
  end

  defp build_invocation(install, argv) do
    capability = install["capability"] || %{}
    commands = ToolRegistry.cli_commands(capability)
    base_invocation = capability["invocation"] || %{}
    namespace = install["command"] || "tool"

    cond do
      commands == [] ->
        {:ok, %{input: Enum.join(argv, " "), invocation: base_invocation}}

      interface_version(capability) > InterfaceManifest.max_cli_version() ->
        {:error,
         {:invalid_arguments,
          "'#{namespace}' needs CLI interface v#{interface_version(capability)}; " <>
            "this arc renders up to v#{InterfaceManifest.max_cli_version()}. Update arc."}}

      true ->
        build_command_invocation(capability, base_invocation, namespace, argv)
    end
  end

  defp interface_version(capability) do
    case InterfaceManifest.cli(capability) do
      %{"version" => v} when is_integer(v) -> v
      _ -> 1
    end
  end

  defp build_command_invocation(capability, base_invocation, namespace, argv) do
    with {:ok, cli_command, remaining_argv} <- resolve_command(capability, argv),
         args = Map.get(cli_command, "args", []),
         {:ok, values} <- parse_cli_args(args, remaining_argv),
         {:ok, input} <- render_input(cli_command, args, values, namespace) do
      invocation = merge_invocation(base_invocation, Map.get(cli_command, "invoke"))
      {:ok, %{input: input, invocation: invocation, command: cli_command}}
    end
  end

  defp invoke_built_command(agent, install, %{input: input, invocation: invocation} = built) do
    case invocation["mode"] || "request_reply" do
      "stream" ->
        invoke_stream_command(agent, install, input, invocation)

      "events" ->
        invoke_events_command(agent, install, built)

      _ ->
        case CapabilityInvocation.invoke(agent, install, input, invocation_override: invocation) do
          {:ok, reply} ->
            reply
            |> apply_output_filter(Map.get(built, :command), Map.get(built, :output_opts, %{}))
            |> print_reply()

          {:error, {:remote, code, message}} ->
            error("tool call failed: #{code}: #{message}")

          {:error, reason} ->
            error("tool call failed: #{inspect(reason)}")
        end
    end
  end

  defp invoke_stream_command(agent, install, input, invocation) do
    input_device = stream_input_device()

    case CapabilityInvocation.open_stream(agent, install, input, invocation_override: invocation) do
      {:ok, stream} ->
        maybe_send_initial_resize(agent, stream, invocation, input_device)
        stdin_task = maybe_start_stream_input(agent, stream, input_device)

        try do
          stream_loop(agent, stream)
        after
          shutdown_input_task(stdin_task)
        end

      {:error, reason} ->
        error("tool call failed: #{inspect(reason)}")
    end
  end

  defp maybe_start_stream_input(agent, stream, input_device) do
    if interactive_stdin?(input_device) do
      Task.async(fn ->
        pump_stream_input(agent, stream, input_device)
      end)
    else
      push_buffered_stream_input(agent, stream, input_device)
      nil
    end
  end

  defp shutdown_input_task(%Task{} = task), do: Task.shutdown(task, :brutal_kill)
  defp shutdown_input_task(_task), do: :ok

  defp pump_stream_input(agent, stream, input_device) do
    Enum.each(IO.stream(input_device, :line), fn line ->
      _ = CapabilityInvocation.send_stream_data(agent, stream, line)
    end)

    _ = CapabilityInvocation.close_stream(agent, stream)
    :ok
  end

  defp push_buffered_stream_input(agent, stream, input_device) do
    drain_buffered_stream_input(agent, stream, input_device)

    _ = CapabilityInvocation.close_stream(agent, stream)
    :ok
  end

  defp drain_buffered_stream_input(agent, stream, input_device) do
    case IO.read(input_device, :line) do
      data when is_binary(data) ->
        _ = CapabilityInvocation.send_stream_data(agent, stream, data)
        drain_buffered_stream_input(agent, stream, input_device)

      :eof ->
        :ok

      {:error, _reason} ->
        :ok
    end
  end

  defp stream_loop(agent, stream) do
    case CapabilityInvocation.recv_stream(agent, stream, timeout_ms: 30_000) do
      {:ok, %{kind: :stream_data} = message} ->
        IO.write(message.text || "")
        stream_loop(agent, stream)

      {:ok, %{kind: :stream_exit, meta: meta} = message} ->
        finish_stream_exit(message.text, meta["status"])

      {:ok, %{kind: :stream_error, error_message: message}} ->
        error("tool call failed: #{message || "stream error"}")

      {:ok, %{kind: :error, error_code: code, error_message: message}} ->
        error("tool call failed: #{code}: #{message}")

      {:ok, %{kind: :response} = reply} ->
        print_reply(reply)

      {:error, :timeout} ->
        error("tool call failed: stream timeout")
    end
  end

  defp finish_stream_exit(text, status) do
    if is_binary(text) and String.trim(text) != "" do
      IO.puts(text)
    end

    case status do
      status when is_integer(status) and status != 0 ->
        error("tool call failed: remote exit status #{status}")

      _ ->
        :ok
    end
  end

  defp maybe_send_initial_resize(agent, stream, invocation, input_device) do
    if get_in(invocation, ["stream", "tty"]) and interactive_stdin?(input_device) do
      case terminal_size() do
        {:ok, cols, rows} -> CapabilityInvocation.resize_stream(agent, stream, cols, rows)
        _ -> :ok
      end
    else
      :ok
    end
  end

  defp terminal_size do
    with {:ok, cols} <- :io.columns(),
         {:ok, rows} <- :io.rows() do
      {:ok, cols, rows}
    else
      _ -> :error
    end
  end

  defp interactive_stdin?(input_device) do
    input_device == :stdio and Process.group_leader() == Process.whereis(:user) and
      match?({:ok, _}, :io.columns()) and
      match?({:ok, _}, :io.rows())
  end

  defp stream_input_device do
    Application.get_env(:arc_cli, :stream_input_device, :stdio)
  end

  defp resolve_command(capability, argv) do
    commands = ToolRegistry.cli_commands(capability)
    root = ToolRegistry.root_command(capability)

    case longest_path_match(commands, argv) do
      nil when commands == [] ->
        {:ok, %{"path" => [], "args" => []}, argv}

      nil when root != nil ->
        {:ok, root, argv}

      nil when argv == [] ->
        {:error, {:invalid_arguments, "missing required subcommand"}}

      nil ->
        {:error, {:invalid_arguments, "unknown subcommand #{hd(argv)}"}}

      cli_command ->
        path = Map.get(cli_command, "path", [])
        {:ok, cli_command, Enum.drop(argv, length(path))}
    end
  end

  defp longest_path_match(commands, argv) do
    commands
    |> Enum.filter(fn cli_command ->
      path = Map.get(cli_command, "path", [])
      path != [] and prefix_match?(argv, path)
    end)
    |> Enum.sort_by(&length(Map.get(&1, "path", [])), :desc)
    |> List.first()
  end

  defp select_help_command(capability, argv) do
    case longest_path_match(ToolRegistry.cli_commands(capability), argv) do
      nil ->
        root = ToolRegistry.root_command(capability)
        subcommands = ToolRegistry.subcommands(capability)

        if root != nil and argv == [] and subcommands == [] do
          {:ok, root}
        else
          :overview
        end

      cli_command ->
        {:ok, cli_command}
    end
  end

  defp prefix_match?(argv, path) when length(argv) < length(path), do: false
  defp prefix_match?(argv, path), do: Enum.take(argv, length(path)) == path

  defp parse_cli_args(args, argv) do
    option_specs =
      args
      |> Enum.filter(&(&1["kind"] == "option"))
      |> Map.new(fn arg -> {arg["flag"], arg} end)

    positional_specs = Enum.filter(args, &(&1["kind"] == "positional"))

    with {:ok, values, positional_tokens} <- collect_option_args(argv, option_specs, %{}, []) do
      assign_positionals(positional_specs, positional_tokens, values)
    end
  end

  defp collect_option_args([], _option_specs, values, positional_tokens) do
    {:ok, values, Enum.reverse(positional_tokens)}
  end

  defp collect_option_args([token | rest], option_specs, values, positional_tokens) do
    cond do
      String.starts_with?(token, "--") and Map.has_key?(option_specs, token) ->
        spec = Map.fetch!(option_specs, token)

        case spec["type"] do
          "boolean" ->
            collect_option_args(
              rest,
              option_specs,
              Map.put(values, spec["name"], true),
              positional_tokens
            )

          _ ->
            case rest do
              [value | tail] ->
                collect_option_args(
                  tail,
                  option_specs,
                  Map.put(values, spec["name"], value),
                  positional_tokens
                )

              [] ->
                {:error, {:invalid_arguments, "missing value for #{token}"}}
            end
        end

      String.starts_with?(token, "--") ->
        {:error, {:invalid_arguments, "unknown option #{token}"}}

      true ->
        collect_option_args(rest, option_specs, values, [token | positional_tokens])
    end
  end

  defp assign_positionals([], [], values), do: {:ok, values}

  defp assign_positionals([], _tokens, _values) do
    {:error, {:invalid_arguments, "too many positional arguments"}}
  end

  defp assign_positionals([spec | rest], tokens, values) do
    cond do
      spec["variadic"] == true ->
        case tokens do
          [] ->
            if spec["required"] do
              {:error, {:invalid_arguments, "missing required argument #{spec["name"]}"}}
            else
              {:ok, values}
            end

          _ ->
            {:ok, Map.put(values, spec["name"], tokens)}
        end

      tokens == [] and spec["required"] ->
        {:error, {:invalid_arguments, "missing required argument #{spec["name"]}"}}

      tokens == [] ->
        assign_positionals(rest, [], values)

      true ->
        [token | tail] = tokens
        assign_positionals(rest, tail, Map.put(values, spec["name"], token))
    end
  end

  defp render_input(%{"input" => %{"source" => "stdin"} = input_spec}, _args, values, namespace) do
    context = filter_context(namespace)

    with {:ok, raw_body} <- read_body(input_spec, values),
         {:ok, body} <- seal_stdin_body(raw_body, input_spec["seal_to"], values, context),
         {:ok, attach_lines} <- attachment_lines(input_spec, values, context) do
      body = Enum.join([body | attach_lines], "\n")

      case input_spec["template"] do
        template when is_binary(template) ->
          with {:ok, header} <- Toolbox.render_template(template, values, context) do
            {:ok, header <> (input_spec["join_with"] || "\n") <> body}
          end

        _ ->
          {:ok, body}
      end
    end
  end

  defp render_input(cli_command, args, values, namespace) do
    Toolbox.render_input(cli_command, args, values, filter_context(namespace))
  end

  @max_attachment_bytes 4 * 1024 * 1024

  # An attachment is sealed once per seal_to target and sent as one line per
  # target: `attach:<name>:<token>`, after the body tokens. The plaintext is
  # capped at 4 MiB so a message to several peers stays inside the 64 MiB
  # request line.
  defp attachment_lines(%{"attach" => arg, "seal_to" => targets}, values, context)
       when is_binary(arg) and is_list(targets) do
    case Map.get(values, arg) do
      path when is_binary(path) and path != "" ->
        full = Path.expand(path)
        name = Path.basename(full)

        with {:ok, bytes} <- File.read(full) |> attach_read_error(full),
             true <-
               byte_size(bytes) <= @max_attachment_bytes or
                 {:error, {:invalid_arguments, "attachment #{name} is over 4 MiB"}},
             true <-
               Regex.match?(~r/^[A-Za-z0-9._-]+$/, name) or
                 {:error,
                  {:invalid_arguments, "attachment name #{name}: letters, digits, . _ - only"}},
             {:ok, tokens} <- seal_stdin_tokens(bytes, targets, values, context) do
          {:ok, Enum.map(tokens, &"attach:#{name}:#{&1}")}
        end

      _ ->
        {:ok, []}
    end
  end

  defp attachment_lines(_input_spec, _values, _context), do: {:ok, []}

  defp attach_read_error({:ok, bytes}, _path), do: {:ok, bytes}

  defp attach_read_error({:error, reason}, path),
    do: {:error, {:invalid_arguments, "cannot read #{path}: #{reason}"}}

  defp seal_stdin_body(body, nil, _values, _context), do: {:ok, body}

  defp seal_stdin_body(body, targets, values, context) when is_list(targets) do
    with {:ok, tokens} <- seal_stdin_tokens(body, targets, values, context) do
      {:ok, Enum.join(tokens, "\n")}
    end
  end

  # One token per peer, targets in order. A target may expand to several
  # peers, so the token count can exceed the target count.
  defp seal_stdin_tokens(body, targets, values, context) do
    Enum.reduce_while(targets, {:ok, []}, fn target, {:ok, acc} ->
      case Toolbox.seal_to(body, target, values, context) do
        {:ok, tokens} -> {:cont, {:ok, acc ++ tokens}}
        {:error, reason} -> {:halt, {:error, {:invalid_arguments, seal_error(target, reason)}}}
      end
    end)
  end

  defp seal_error(target, {:resolve, value, reason}),
    do: "cannot seal to #{target}: #{value} #{reason}"

  defp seal_error(target, {:no_keyex, value}),
    do: "cannot seal to #{target}: #{value} has no published X25519 key"

  defp seal_error(target, reason), do: "cannot seal to #{target}: #{inspect(reason)}"

  # Filters that resolve names or seal bodies need the control plane and
  # the caller's identity. Both are cheap to look up per invocation.
  defp filter_context(namespace \\ nil) do
    identity =
      case KeyStore.resolve_active() do
        {:ok, id} -> id
        _ -> nil
      end

    lists =
      if is_binary(namespace), do: &Arc.CLI.Lists.expand(namespace, &1), else: fn _ -> nil end

    %{resolve: &Arc.Control.resolve/1, identity: identity, lists: lists}
  end

  # The body comes from a named argument, then from a file named by an
  # option, then from stdin. Only the last blocks on a terminal.
  defp read_body(input_spec, values) do
    body_arg = input_spec["body"]
    file_arg = input_spec["file"]

    cond do
      is_binary(body_arg) and present_value?(Map.get(values, body_arg)) ->
        {:ok, values |> Map.get(body_arg) |> join_value()}

      is_binary(file_arg) and is_binary(Map.get(values, file_arg)) ->
        path = Path.expand(Map.get(values, file_arg))

        case File.read(path) do
          {:ok, body} -> {:ok, body}
          {:error, reason} -> {:error, {:invalid_arguments, "cannot read #{path}: #{reason}"}}
        end

      true ->
        read_stdin_body()
    end
  end

  defp present_value?(nil), do: false
  defp present_value?([]), do: false
  defp present_value?(""), do: false
  defp present_value?(_), do: true

  defp join_value(values) when is_list(values), do: Enum.join(values, " ")
  defp join_value(value), do: to_string(value)

  defp read_stdin_body do
    case IO.read(stream_input_device(), :eof) do
      :eof ->
        {:ok, ""}

      {:error, reason} ->
        {:error, {:invalid_arguments, "failed to read stdin: #{inspect(reason)}"}}

      body when is_binary(body) ->
        {:ok, body}
    end
  end

  defp merge_invocation(base, override) when is_map(override) do
    Map.merge(base, override)
  end

  defp merge_invocation(base, _override), do: base

  defp extract_help(argv) do
    case Enum.reverse(argv) do
      ["--help" | rest] -> {true, Enum.reverse(rest)}
      _ -> {false, argv}
    end
  end

  defp usage_label(%{"kind" => "option", "flag" => flag, "type" => "boolean"}) do
    flag
  end

  defp usage_label(%{"name" => name, "kind" => "option", "flag" => flag}) do
    flag <> " <" <> name <> ">"
  end

  defp usage_label(%{"name" => name, "variadic" => true}) do
    "<" <> name <> "...>"
  end

  defp usage_label(%{"name" => name}) do
    "<" <> name <> ">"
  end

  defp fetch_remote_package(agent, install) do
    peer = get_in(install, ["provider", "name"]) || get_in(install, ["provider", "short_name"])
    capability_id = install["capability_id"] || get_in(install, ["capability", "id"])

    with true <- (is_binary(peer) and peer != "") or {:error, :invalid_install},
         true <- (is_binary(capability_id) and capability_id != "") or {:error, :invalid_install},
         {:ok, detail} <- CapabilityDiscovery.fetch_detail(agent, peer, capability_id),
         {:ok, verified} <- CapabilityPackage.verify(detail) do
      {:ok, verified}
    else
      {:error, _reason} = error -> error
    end
  end

  defp ensure_same_signer(tool, remote) do
    if tool["signer_public_key"] == get_in(remote, ["signature", "signer_public_key"]) do
      :ok
    else
      {:error, :signer_changed}
    end
  end

  defp ensure_not_pinned(%{"pinned" => true}), do: {:error, :pinned}
  defp ensure_not_pinned(_tool), do: :ok

  defp ensure_trusted(owner, verified, opts) do
    signer_public_key = get_in(verified, ["signature", "signer_public_key"])

    case TrustStore.get(owner, signer_public_key) do
      {:ok, %{"state" => "allowed"}} ->
        {:ok, "allowed"}

      {:ok, %{"state" => "denied"}} ->
        {:error, {:signer_denied, signer_public_key}}

      {:error, :not_found} ->
        if opts[:trust] do
          allow_signer(owner, signer_public_key)
        else
          prompt_trust(owner, verified)
        end

      {:error, reason} ->
        {:error, reason}
    end
  end

  defp prompt_trust(owner, verified) do
    provider = verified["provider"] || %{}
    capability = verified["capability"] || %{}
    signer_public_key = get_in(verified, ["signature", "signer_public_key"])

    IO.puts("New signer for #{Identity.name(owner)}")
    IO.puts("  Provider: #{provider["name"] || provider["short_name"] || "unknown"}")

    IO.puts(
      "  Capability: #{capability["id"] || "unknown"} [#{capability["kind"]}/#{capability["scheme"]}]"
    )

    IO.puts(
      "  Version: #{get_in(verified, ["release", "version"])} (#{get_in(verified, ["release", "channel"])})"
    )

    IO.puts("  Signer: #{signer_public_key}")
    IO.puts("  Hash: #{verified["package_hash"]}")

    case trust_answer(IO.gets("Trust this signer? [y/N]: ")) do
      :allow ->
        allow_signer(owner, signer_public_key)

      :deny ->
        with {:ok, _record} <- TrustStore.deny(owner, signer_public_key) do
          {:error, {:trust_denied, signer_public_key}}
        end

      :cancel ->
        {:error, :trust_declined}
    end
  end

  defp allow_signer(owner, signer_public_key) do
    with {:ok, _record} <- TrustStore.allow(owner, signer_public_key) do
      {:ok, "allowed"}
    end
  end

  defp short_key(nil), do: "unknown"
  defp short_key(value) when byte_size(value) <= 12, do: value

  defp short_key(value),
    do: binary_part(value, 0, 8) <> "…" <> binary_part(value, byte_size(value), -8)

  defp short_hash(nil), do: "unknown"
  defp short_hash(value) when byte_size(value) <= 12, do: value

  defp short_hash(value),
    do: binary_part(value, 0, 8) <> "…" <> binary_part(value, byte_size(value), -8)

  defp active_identity do
    KeyStore.resolve_active()
  end

  defp with_identity(fun) do
    case active_identity() do
      {:ok, id} ->
        fun.(id)

      {:error, :no_default} ->
        error("No active key. Run 'arc keys gen' first.")

      {:error, :not_found} ->
        error("ARC_KEY='#{System.get_env("ARC_KEY")}' not found in key store.")

      {:error, reason} ->
        error(inspect(reason))
    end
  end

  defp with_agent(fun, opts) do
    with_identity(fn id ->
      case Agent.start_link(id) do
        {:ok, agent} ->
          try do
            :ok = Agent.publish(agent)
            maybe_connect_relay(id, opts)
            fun.(agent, id)
          after
            if Process.alive?(agent), do: GenServer.stop(agent, :normal)
          end

        {:error, reason} ->
          error("failed to start agent: #{inspect(reason)}")
      end
    end)
  end

  defp maybe_connect_relay(my_identity, opts) do
    relay_addr =
      case Keyword.get(opts, :relay) do
        nil -> Arc.Net.relay_address()
        addr -> Arc.Net.relay_address_from(addr)
      end

    relay_pubkey_pin = resolve_relay_pubkey_pin(opts)

    case relay_addr do
      {host, port} ->
        case Arc.Net.connect_relay(host, port, my_identity, relay_pubkey_pin) do
          :ok -> :ok
          {:error, reason} -> IO.puts(:stderr, "relay connect failed: #{inspect(reason)}")
        end

      nil ->
        :ok
    end
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

  defp pop_opt(args, flag), do: pop_opt(args, flag, [])

  defp pop_opt([flag, value | rest], flag, acc) do
    {value, Enum.reverse(acc) ++ rest}
  end

  defp pop_opt([h | rest], flag, acc) do
    pop_opt(rest, flag, [h | acc])
  end

  defp pop_opt([], _flag, acc), do: {nil, Enum.reverse(acc)}

  defp pop_flag(args, flag) do
    {Enum.member?(args, flag), Enum.reject(args, &(&1 == flag))}
  end

  @spec error(String.t()) :: no_return()
  defp error(msg) do
    IO.puts(:stderr, "error: #{msg}")
    Arc.CLI.Exit.halt(1)
  end
end
