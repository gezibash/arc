defmodule Arc.CLI.Agent do
  @moduledoc """
  CLI commands for agent messaging and serving.
  """

  alias Arc.Identity
  alias Arc.Identity.KeyStore
  alias Arc.Data.Agent
  alias Arc.Data.CapabilityDiscovery
  alias Arc.Data.CapabilityPackage
  alias Arc.Data.CapabilityInvocation
  alias Arc.Data.Frame
  alias Arc.CLI.ProviderBundle
  alias Arc.CLI.ServeView
  alias Arc.Host.Client
  alias Arc.Host.Service
  alias Arc.Host.Token
  alias Arc.MCP.DynamicToolRegistry

  def run(args) do
    {relay_pubkey, args} = pop_opt(args, "--relay-pubkey")
    {relay_addr, clean_args} = pop_opt(args, "--relay")
    opts = [relay: relay_addr, relay_pubkey: relay_pubkey]
    dispatch(clean_args, opts)
  end

  defp dispatch(["send", to, message | _], opts) do
    with_agent(
      fn agent, _id ->
        case connect_peer(agent, to) do
          {:ok, entry} ->
            pk_short = short_public_key(entry.public_key)
            request_id = Frame.new_request_id()

            case Agent.send_message(agent, to, message,
                   request_id: request_id,
                   meta: %{"method" => "RAW", "path" => "/"}
                 ) do
              :ok ->
                IO.puts("Sent to #{entry.name} (#{pk_short})")
                wait_for_reply(agent, request_id)

              {:error, reason} ->
                error("send failed: #{inspect(reason)}")
            end

          {:error, :not_found} ->
            error("no identity found for '#{to}'")

          {:error, :no_keyex} ->
            error("peer '#{to}' has no key exchange material published")

          {:error, reason} ->
            error("connect failed: #{inspect(reason)}")
        end
      end,
      opts
    )
  end

  defp dispatch(["discover" | query_parts], opts) do
    query = Enum.join(query_parts, " ")

    with_agent(
      fn agent, _id ->
        case CapabilityDiscovery.discover(agent, query) do
          {:ok, result} ->
            print_discovery(result)

          {:error, reason} ->
            error("discover failed: #{inspect(reason)}")
        end
      end,
      opts
    )
  end

  defp dispatch(["mount", task, "add", peer, capability_id | _], opts) do
    with_agent(
      fn agent, id ->
        case CapabilityDiscovery.fetch_detail(agent, peer, capability_id) do
          {:ok, detail} ->
            case DynamicToolRegistry.mount(id, task, detail) do
              {:ok, mount} ->
                print_mount_added(task, mount)

              {:error, {:mount_limit_exceeded, limit}} ->
                error("mount failed: task '#{task}' already has #{limit} mounted capabilities")

              {:error, reason} ->
                error("mount failed: #{inspect(reason)}")
            end

          {:error, {:remote, code, message}} ->
            error("mount failed: #{code}: #{message}")

          {:error, reason} ->
            error("mount failed: #{inspect(reason)}")
        end
      end,
      opts
    )
  end

  defp dispatch(["mount", task, "ls" | _], _opts) do
    with_identity(fn id ->
      case DynamicToolRegistry.list(id, task) do
        {:ok, mounts} -> print_mounts(id, task, mounts)
        {:error, reason} -> error("mount ls failed: #{inspect(reason)}")
      end
    end)
  end

  defp dispatch(["mount", task, "call", peer, capability_id | input_parts], opts) do
    input = Enum.join(input_parts, " ")

    with_agent(
      fn agent, id ->
        with {:ok, mount} <- DynamicToolRegistry.get(id, task, peer, capability_id),
             {:ok, reply} <- CapabilityInvocation.invoke(agent, mount, input) do
          print_message(reply)
        else
          {:error, :not_found} ->
            error("mount call failed: #{peer}/#{capability_id} is not mounted in task '#{task}'")

          {:error, {:remote, code, message}} ->
            error("mount call failed: #{code}: #{message}")

          {:error, reason} ->
            error("mount call failed: #{inspect(reason)}")
        end
      end,
      opts
    )
  end

  defp dispatch(["mount", task, "rm", peer, capability_id | _], _opts) do
    with_identity(fn id ->
      case DynamicToolRegistry.unmount(id, task, peer, capability_id) do
        :ok ->
          IO.puts("Unmounted #{task}: #{peer}/#{capability_id}")

        {:error, reason} ->
          error("mount rm failed: #{inspect(reason)}")
      end
    end)
  end

  defp dispatch(["info", to], opts) do
    with_agent(
      fn agent, _id ->
        case connect_peer(agent, to) do
          {:ok, entry} ->
            case request_document(agent, to, "/info") do
              {:ok, document} ->
                print_summary(document, entry)

              {:error, {:remote, code, message}} ->
                error("info failed: #{code}: #{message}")

              {:error, reason} ->
                error("info failed: #{inspect(reason)}")
            end

          {:error, :not_found} ->
            error("no identity found for '#{to}'")

          {:error, :no_keyex} ->
            error("peer '#{to}' has no key exchange material published")

          {:error, reason} ->
            error("connect failed: #{inspect(reason)}")
        end
      end,
      opts
    )
  end

  defp dispatch(["info", to, capability_id | _], opts) do
    with_agent(
      fn agent, _id ->
        case connect_peer(agent, to) do
          {:ok, entry} ->
            path = "/info/capabilities/" <> capability_id

            case request_document(agent, to, path) do
              {:ok, document} ->
                print_detail(document, entry)

              {:error, {:remote, code, message}} ->
                error("info failed: #{code}: #{message}")

              {:error, reason} ->
                error("info failed: #{inspect(reason)}")
            end

          {:error, :not_found} ->
            error("no identity found for '#{to}'")

          {:error, :no_keyex} ->
            error("peer '#{to}' has no key exchange material published")

          {:error, reason} ->
            error("connect failed: #{inspect(reason)}")
        end
      end,
      opts
    )
  end

  defp dispatch(["listen" | _], opts) do
    with_agent(
      fn agent, id ->
        pk_hex = Identity.encode_public_key(id)
        pk_short = binary_part(pk_hex, 0, 4) <> "…" <> binary_part(pk_hex, byte_size(pk_hex), -4)
        IO.puts("Listening as #{Identity.name(id)} (#{pk_short}) [pid:#{System.pid()}]")
        IO.puts("Press Ctrl+C to stop.\n")
        listen_loop(agent)
      end,
      opts
    )
  end

  defp dispatch(["serve", target | _], opts) do
    {resolved, bundle} =
      case ProviderBundle.resolve_serve_target(target) do
        {:ok, uri, bundle} ->
          {uri, bundle}

        {:error, :arcfile_not_found} ->
          error("serve failed: no Arcfile found at #{target}")

        {:error, :manifest_not_found} ->
          error("serve failed: Arcfile points to a missing manifest")

        {:error, reason} ->
          error("serve failed: #{inspect(reason)}")
      end

    with_serving_agent(
      resolved,
      fn agent, id, relay_info ->
        serve_meta = serve_meta(target, resolved, bundle)
        IO.puts(ServeView.render_banner(id, serve_meta, relay_info))
        IO.puts("")
        listen_loop(agent)
      end,
      opts
    )
  end

  defp dispatch(_, _opts) do
    IO.puts("""
    arc agent commands

    Commands:
      discover [query]       Search remote capability summaries
      mount <task> ...       Manage task-scoped mounted capabilities
      send <to> <message>    Send a message (waits for reply)
      info <peer> [id]       Fetch remote capability summary or detail
      listen                 Listen for incoming messages
      serve <target>         Serve a provider bundle or low-level runtime URI

    Options:
      --relay host:port      Connect through a relay node
      --relay-pubkey <key>   Pin relay identity pubkey (hex/base64)

    Serve targets:
      /path/to/bundle-dir
      /path/to/bundle-dir/Arcfile
      exec:///path/to/runtime?manifest=/abs/path/to/manifest.(json|toml)

    Mount commands:
      mount <task> add <peer> <id>
      mount <task> ls
      mount <task> call <peer> <id> [input]
      mount <task> rm <peer> <id>
    """)
  end

  defp request_document(agent, to, path) do
    request_id = Frame.new_request_id()

    case Agent.send_message(agent, to, "",
           request_id: request_id,
           meta: %{"method" => "GET", "path" => path}
         ) do
      :ok ->
        with {:ok, msg} <- wait_for_reply_message(agent, request_id),
             {:ok, document} <- decode_document(msg) do
          {:ok, document}
        end

      {:error, reason} ->
        {:error, reason}
    end
  end

  defp wait_for_reply(agent, request_id) do
    wait_for_reply(agent, request_id, 0)
  end

  defp wait_for_reply(_agent, _request_id, elapsed) when elapsed > 10_000 do
    :ok
  end

  defp wait_for_reply(agent, request_id, elapsed) do
    case wait_for_reply_message(agent, request_id, elapsed) do
      {:ok, match} -> print_message(match)
      {:error, :timeout} -> :ok
    end
  end

  defp wait_for_reply_message(agent, request_id) do
    wait_for_reply_message(agent, request_id, 0)
  end

  defp wait_for_reply_message(_agent, _request_id, elapsed) when elapsed > 10_000 do
    {:error, :timeout}
  end

  defp wait_for_reply_message(agent, request_id, elapsed) do
    Process.sleep(100)
    Agent.poll_mailbox(agent)
    Process.sleep(10)

    matcher = fn message ->
      message[:request_id] == request_id and message[:kind] in [:response, :error]
    end

    case Agent.take_inbox(agent, matcher) do
      [] ->
        wait_for_reply_message(agent, request_id, elapsed + 110)

      [match | _] ->
        {:ok, match}
    end
  end

  defp decode_document(%{kind: :error, error_code: code, error_message: message}) do
    {:error, {:remote, code || "error", message || "unknown error"}}
  end

  defp decode_document(%{kind: :response, text: text}) do
    try do
      {:ok, :json.decode(text)}
    rescue
      _ -> {:error, :invalid_json}
    end
  end

  defp decode_document(_msg), do: {:error, :unexpected_reply}

  defp listen_loop(agent) do
    receive do
      {:arc_serve_event, event} ->
        IO.puts(ServeView.render_event(event))
        drain_serve_events()
    after
      500 -> :ok
    end

    Agent.poll_mailbox(agent)
    Process.sleep(10)
    drain_serve_events()

    case Agent.read_inbox(agent) do
      [] ->
        :ok

      messages ->
        Enum.each(messages, &print_message/1)
    end

    listen_loop(agent)
  end

  defp drain_serve_events do
    receive do
      {:arc_serve_event, event} ->
        IO.puts(ServeView.render_event(event))
        drain_serve_events()
    after
      0 -> :ok
    end
  end

  defp print_message(msg) do
    kind = msg[:kind] || :raw
    text = msg[:text] || ""
    from = msg[:from] || "unknown"

    case kind do
      :error ->
        IO.puts("[#{from}] #{text}")

      :response ->
        IO.puts(text)

      _ ->
        IO.puts("[#{from}] #{text}")
    end
  end

  defp print_summary(%{"provider" => provider, "capabilities" => capabilities}, entry)
       when is_list(capabilities) do
    IO.puts("Provider: #{provider_label(provider)} (#{short_public_key(entry.public_key)})")
    IO.puts("Capabilities: #{length(capabilities)}")

    Enum.each(capabilities, fn capability ->
      id = capability["id"] || "unknown"
      kind = capability["kind"] || "capability"
      scheme = capability["scheme"] || "unknown"
      title = capability["title"] || id
      summary = capability["summary"] || ""
      detail_path = capability["detail_path"] || ""

      IO.puts("")
      IO.puts("#{id} [#{kind}/#{scheme}]")
      IO.puts("  #{title}")

      if summary != "" do
        IO.puts("  #{summary}")
      end

      if detail_path != "" do
        IO.puts("  Expand: arc info #{provider["name"] || entry.name} #{id}")
      end
    end)
  end

  defp print_summary(document, _entry) do
    IO.puts(inspect(document, pretty: true, limit: :infinity))
  end

  defp print_detail(%{"provider" => provider, "capability" => capability} = document, entry)
       when is_map(capability) do
    capability = attach_interfaces(capability, document)

    IO.puts("Provider: #{provider_label(provider)} (#{short_public_key(entry.public_key)})")
    IO.puts("Capability: #{capability["id"] || "unknown"}")
    IO.puts("Type: #{capability["kind"] || "capability"}/#{capability["scheme"] || "unknown"}")
    IO.puts("Version: #{get_in(document, ["release", "version"]) || "unknown"}")
    IO.puts("Channel: #{get_in(document, ["release", "channel"]) || "unknown"}")

    if present?(capability["title"]) do
      IO.puts("Title: #{capability["title"]}")
    end

    if present?(capability["summary"]) do
      IO.puts("Summary: #{capability["summary"]}")
    end

    invocation = capability["invocation"] || %{}
    IO.puts("Invocation: #{invocation["method"] || "RAW"} #{invocation["path"] || "/"}")

    case Map.get(document, "package_hash") do
      hash when is_binary(hash) and hash != "" -> IO.puts("Hash: #{hash}")
      _ -> :ok
    end

    case get_in(document, ["signature", "signer_public_key"]) do
      signer when is_binary(signer) and signer != "" -> IO.puts("Signer: #{signer}")
      _ -> :ok
    end

    if is_map(capability["config"]) and map_size(capability["config"]) > 0 do
      IO.puts("Config:")

      capability["config"]
      |> Enum.sort_by(fn {key, _value} -> to_string(key) end)
      |> Enum.each(fn {key, value} ->
        IO.puts("  #{key}: #{format_value(value)}")
      end)
    end

    examples = capability["examples"] || []

    if is_list(examples) and examples != [] do
      IO.puts("Examples:")
      Enum.each(examples, fn example -> IO.puts("  #{example}") end)
    end

    provider_name = provider["name"] || entry.name
    capability_id = capability["id"] || "unknown"

    case Arc.CLI.ToolRegistry.cli_interface(capability) do
      %{"namespace" => name} = cli ->
        IO.puts("Install: arc install #{provider_name} #{capability_id}")
        IO.puts("Install alias: #{name}")

        case Map.get(cli, "commands", []) do
          [] ->
            :ok

          commands ->
            IO.puts("Commands:")

            Enum.each(commands, fn command ->
              label = Arc.CLI.ToolRegistry.command_label(command)
              summary = command["summary"] || ""
              IO.puts("  #{label}#{if(summary == "", do: "", else: "  " <> summary)}")
            end)
        end

      _ ->
        IO.puts("Install: not published")
    end
  end

  defp print_detail(document, _entry) do
    IO.puts(inspect(document, pretty: true, limit: :infinity))
  end

  defp print_discovery(%{query: query, total: total, truncated?: truncated?, matches: matches}) do
    query_label = if query == "", do: "(all)", else: query
    IO.puts("Query: #{query_label}")

    if truncated? do
      IO.puts("Matches: #{length(matches)} shown of #{total}")
    else
      IO.puts("Matches: #{total}")
    end

    Enum.each(matches, fn %{provider: provider, capability: capability} ->
      provider_name = provider["name"] || provider["short_name"] || "unknown"
      id = capability["id"] || "unknown"
      kind = capability["kind"] || "capability"
      scheme = capability["scheme"] || "unknown"
      title = capability["title"] || id
      summary = capability["summary"] || ""

      IO.puts("")
      IO.puts("#{provider_name}/#{id} [#{kind}/#{scheme}]")
      IO.puts("  #{title}")

      if summary != "" do
        IO.puts("  #{summary}")
      end

      IO.puts("  Expand: arc info #{provider_name} #{id}")
      IO.puts("  Mount:  arc mount <task> add #{provider_name} #{id}")

      if Arc.CLI.ToolRegistry.cli_interface(capability) do
        IO.puts("  Install: arc install #{provider_name} #{id}")
      end
    end)
  end

  defp print_mount_added(task, mount) do
    provider = mount["provider"] || %{}
    capability = mount["capability"] || %{}
    provider_name = provider["name"] || provider["short_name"] || "unknown"
    id = capability["id"] || "unknown"
    kind = capability["kind"] || "capability"
    scheme = capability["scheme"] || "unknown"

    IO.puts("Mounted #{task}: #{provider_name}/#{id} [#{kind}/#{scheme}]")
  end

  defp print_mounts(owner, task, mounts) when is_list(mounts) do
    IO.puts("Owner: #{Identity.name(owner)}")
    IO.puts("Task: #{task}")
    IO.puts("Mounted: #{length(mounts)}")

    Enum.each(mounts, fn mount ->
      provider = mount["provider"] || %{}
      capability = mount["capability"] || %{}
      provider_name = provider["name"] || provider["short_name"] || "unknown"
      id = capability["id"] || "unknown"
      kind = capability["kind"] || "capability"
      scheme = capability["scheme"] || "unknown"
      invocation = capability["invocation"] || %{}

      IO.puts("")
      IO.puts("#{provider_name}/#{id} [#{kind}/#{scheme}]")

      if present?(capability["title"]) do
        IO.puts("  #{capability["title"]}")
      end

      IO.puts("  Invocation: #{invocation["method"] || "RAW"} #{invocation["path"] || "/"}")
    end)
  end

  defp provider_label(provider) when is_map(provider) do
    provider["name"] || provider["short_name"] || "unknown"
  end

  defp provider_label(_provider), do: "unknown"

  defp present?(value) when is_binary(value), do: value != ""
  defp present?(_value), do: false

  defp format_value(value) when is_binary(value), do: value
  defp format_value(value), do: inspect(value)

  defp connect_peer(agent, to) do
    Agent.connect(agent, to)
  end

  defp short_public_key(public_key) do
    pk_hex = Base.encode16(public_key, case: :lower)
    binary_part(pk_hex, 0, 4) <> "…" <> binary_part(pk_hex, byte_size(pk_hex), -4)
  end

  defp with_identity(fun) do
    case KeyStore.resolve_active() do
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

  defp with_serving_agent(uri, fun, opts) do
    case KeyStore.resolve_active() do
      {:ok, id} ->
        serve_uri = attach_host_runtime_env(uri, id)

        case Agent.start_link(id, serve: serve_uri, observer: self()) do
          {:ok, agent} ->
            :ok = Agent.publish(agent)
            relay_info = maybe_connect_relay(id, opts)
            fun.(agent, id, relay_info)

          {:error, {:handler_init_failed, _uri, {:missing_capability_manifest, mod}}} ->
            error("serve failed: #{inspect(mod)} must publish an explicit capability manifest")

          {:error, {:handler_init_failed, _uri, reason}} ->
            error("serve failed: #{inspect(reason)}")

          {:error, reason} ->
            error("serve failed: #{inspect(reason)}")
        end

      {:error, :no_default} ->
        error("No active key. Run 'arc keys gen' first.")

      {:error, :not_found} ->
        error("ARC_KEY='#{System.get_env("ARC_KEY")}' not found in key store.")

      {:error, reason} ->
        error(inspect(reason))
    end
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
          :ok ->
            %{
              host: List.to_string(host),
              port: port,
              pubkey_pin: relay_pubkey_pin,
              connected?: true
            }

          {:error, reason} ->
            IO.puts(:stderr, "relay connect failed: #{inspect(reason)}")

            %{
              host: List.to_string(host),
              port: port,
              pubkey_pin: relay_pubkey_pin,
              connected?: false,
              error: reason
            }
        end

      nil ->
        nil
    end
  end

  defp serve_meta(target, resolved, bundle) do
    manifest_path =
      cond do
        is_map(bundle) -> bundle.manifest.path
        true -> manifest_path_from_uri(resolved)
      end

    runtime_path =
      cond do
        is_map(bundle) -> bundle.runtime.command
        true -> runtime_path_from_uri(resolved)
      end

    package =
      case manifest_path do
        path when is_binary(path) ->
          case CapabilityPackage.load_file(path) do
            {:ok, package} -> package
            _ -> %{}
          end

        _ ->
          %{}
      end

    capability =
      package
      |> Map.get("capability", %{})
      |> attach_interfaces(package)

    %{
      target: target,
      resolved: resolved,
      scheme: URI.parse(resolved).scheme,
      bundle_root: if(is_map(bundle), do: bundle.root, else: nil),
      runtime_path: runtime_path,
      manifest_path: manifest_path,
      capability: capability,
      command_usages: ServeView.command_usages(capability)
    }
  end

  defp attach_host_runtime_env(uri, %Identity{} = identity) when is_binary(uri) do
    case URI.parse(uri) do
      %URI{scheme: "exec"} = parsed ->
        host_env = host_runtime_env(identity)

        query =
          parsed.query
          |> decode_query()
          |> Map.merge(host_env)
          |> URI.encode_query()

        %{parsed | query: query}
        |> URI.to_string()

      _ ->
        uri
    end
  end

  defp attach_host_runtime_env(uri, _identity), do: uri

  defp host_runtime_env(%Identity{} = identity) do
    socket_path = Service.default_socket_path()

    base = %{
      "arc_identity" => Identity.name(identity),
      "arc_identity_short" => Identity.short_name(identity),
      "arc_public_key" => Identity.encode_public_key(identity)
    }

    case issue_runtime_host_token(socket_path, identity) do
      {:ok, token} ->
        Map.merge(base, %{
          "arc_host_socket" => socket_path,
          "arc_host_token" => token
        })

      {:error, _reason} ->
        base
    end
  end

  defp issue_runtime_host_token(socket_path, %Identity{} = identity) do
    with {:ok, admin_token} <- Service.read_admin_token(socket_path),
         {:ok, issued} <-
           Client.request(
             socket_path,
             "token.issue",
             %{
               "identity" => Identity.name(identity),
               "label" => "serve:#{Identity.name(identity)}",
               "scopes" => Token.delegated_scopes()
             },
             token: admin_token
           ) do
      case issued["token"] do
        token when is_binary(token) and token != "" -> {:ok, token}
        _ -> {:error, :missing_token}
      end
    else
      {:error, _reason} = error -> error
    end
  end

  defp attach_interfaces(capability, %{"interfaces" => interfaces})
       when is_map(capability) and is_map(interfaces) do
    Map.put_new(capability, "interfaces", interfaces)
  end

  defp attach_interfaces(capability, _document), do: capability

  defp manifest_path_from_uri(uri) when is_binary(uri) do
    uri
    |> URI.parse()
    |> Map.get(:query)
    |> case do
      nil -> nil
      query -> URI.decode_query(query)["manifest"]
    end
  end

  defp decode_query(nil), do: %{}
  defp decode_query(""), do: %{}
  defp decode_query(query), do: URI.decode_query(query)

  defp runtime_path_from_uri(uri) when is_binary(uri) do
    case URI.parse(uri) do
      %URI{path: path} when is_binary(path) and path != "" -> URI.decode(path)
      _ -> nil
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

  defp pop_opt([], _flag, acc) do
    {nil, Enum.reverse(acc)}
  end

  defp error(msg) do
    IO.puts(:stderr, "error: #{msg}")
    System.halt(1)
  end
end
