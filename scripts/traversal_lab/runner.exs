#!/usr/bin/env elixir

# Long-lived process harness for the remote traversal lab.  Its only output is
# line-delimited JSON prefixed with ARC_LAB so the orchestrator can retain safe
# evidence without collecting key material, direct-route state, or request
# bodies.  Inputs are line-delimited JSON commands on stdin.

defmodule Arc.TraversalLab.Runner do
  alias Arc.Data.Agent
  alias Arc.Data.Direct
  alias Arc.Data.Protocol
  alias Arc.Identity
  alias Arc.Identity.KeyStore
  alias Arc.Net.Relay

  @prefix "ARC_LAB "
  @identity_envs ["ARC_KEY", "ARC_RELAY", "ARC_RELAY_PUBKEY", "ARC_RELAY_KEY", "ARC_RELAY_PORT"]

  def main(argv) do
    case argv do
      ["--" | args] -> main(args)
      ["initialize", state_root] -> initialize(state_root)
      ["relay", config_path] -> relay(config_path)
      ["provider", config_path] -> endpoint(:provider, config_path)
      ["citizen", config_path] -> endpoint(:citizen, config_path)
      _ -> usage()
    end
  rescue
    error ->
      emit(%{"event" => "fatal", "error" => safe_error(error)})
      System.halt(1)
  end

  # Creates or reuses an isolated identity in state_root.  The seed remains in
  # the local key store; callers receive only the public key needed to write the
  # peer's exact direct-policy file.
  defp initialize(state_root) do
    configure_state(state_root)
    Application.ensure_all_started(:arc_identity)

    identity =
      case KeyStore.resolve_active() do
        {:ok, existing} ->
          existing

        {:error, :no_default} ->
          {:ok, generated} = KeyStore.generate()
          :ok = KeyStore.set_default(Identity.name(generated))
          generated

        {:error, reason} ->
          raise "identity initialization failed: #{inspect(reason)}"
      end

    emit(%{
      "event" => "initialized",
      "public_key" => Identity.encode_public_key(identity)
    })
  end

  defp relay(config_path) do
    config = load_config(config_path, ["port", "public_key"])
    port = integer!(config["port"], "port", 1, 65_535)
    relay_key = hex_key!(config["public_key"], "public_key")
    Application.ensure_all_started(:arc_net)
    {:ok, relay} = Relay.start_link(port, relay_public_key: relay_key)

    try do
      emit(%{"event" => "ready", "role" => "relay", "port" => Relay.get_port(relay)})
      relay_commands(relay)
    after
      if Process.alive?(relay), do: GenServer.stop(relay, :normal)
    end
  end

  defp endpoint(role, config_path) do
    config = load_config(config_path, ["state_root", "relay", "direct_policy"])
    state_root = string!(config["state_root"], "state_root")
    configure_state(state_root)
    Application.ensure_all_started(:arc_data)
    Application.ensure_all_started(:arc_net)

    {:ok, identity} = KeyStore.resolve_active()
    {:ok, policy} = Direct.Policy.load_file(string!(config["direct_policy"], "direct_policy"))
    {relay_host, relay_port, relay_key} = relay_config!(config["relay"])
    serve = if role == :provider, do: string!(config["serve"], "serve"), else: nil
    {:ok, agent} = Agent.start_link(identity, direct_policy: policy, serve: serve)

    try do
      :ok =
        Arc.Net.acquire_relay(String.to_charlist(relay_host), relay_port, identity, relay_key,
          owner_pid: agent
        )

      :ok = Agent.publish_relay(agent)

      emit(%{
        "event" => "ready",
        "role" => Atom.to_string(role),
        "public_key" => Identity.encode_public_key(identity)
      })

      endpoint_commands(role, agent, config)
    after
      Arc.Net.release_relay(identity.public_key, agent)
      if Process.alive?(agent), do: GenServer.stop(agent, :normal)
    end
  end

  defp relay_commands(relay) do
    Enum.each(IO.stream(:stdio, :line), fn line ->
      case command(line) do
        %{"op" => "status"} ->
          stats = Relay.stats(relay)

          emit(%{
            "event" => "status",
            "role" => "relay",
            "routes" => stats.routes,
            "connections" => stats.conns
          })

        %{"op" => "shutdown"} ->
          throw(:shutdown)

        _ ->
          emit(%{"event" => "error", "error" => "invalid_command"})
      end
    end)
  catch
    :throw, :shutdown -> :ok
  end

  defp endpoint_commands(:provider, agent, _config) do
    Enum.each(IO.stream(:stdio, :line), fn line ->
      case command(line) do
        %{"op" => "status"} -> emit(endpoint_status("provider", agent, nil))
        %{"op" => "shutdown"} -> throw(:shutdown)
        _ -> emit(%{"event" => "error", "error" => "invalid_command"})
      end
    end)
  catch
    :throw, :shutdown -> :ok
  end

  defp endpoint_commands(:citizen, agent, config) do
    address = string!(Map.get(config, "address"), "address")
    capability = Map.get(config, "capability", "primary")
    timeout = Map.get(config, "request_timeout_ms", 10_000)

    if not (is_binary(capability) and is_integer(timeout) and timeout in 1..120_000),
      do: raise("invalid request configuration")

    Enum.each(IO.stream(:stdio, :line), fn line ->
      case command(line) do
        %{"op" => "request", "body" => body} when is_binary(body) ->
          started = System.monotonic_time(:millisecond)

          result =
            Protocol.request(agent, address, body, timeout_ms: timeout, capability: capability)

          elapsed = System.monotonic_time(:millisecond) - started
          emit(request_status(agent, address, result, elapsed))

        %{"op" => "status"} ->
          emit(endpoint_status("citizen", agent, address))

        %{"op" => "shutdown"} ->
          throw(:shutdown)

        _ ->
          emit(%{"event" => "error", "error" => "invalid_command"})
      end
    end)
  catch
    :throw, :shutdown -> :ok
  end

  defp request_status(agent, address, result, elapsed) do
    %{
      "event" => "request",
      "elapsed_ms" => elapsed,
      "result" =>
        case result do
          {:ok, %{body: body}} -> response_summary(body)
          {:error, reason} -> %{"ok" => false, "error" => safe_reason(reason)}
        end,
      "direct" => direct_status(agent, address)
    }
  end

  defp endpoint_status(role, agent, address) do
    %{
      "event" => "status",
      "role" => role,
      "direct" => if(address, do: direct_status(agent, address), else: route_summary(agent)),
      "relay_endpoint" => relay_endpoint(agent)
    }
  end

  # The lab needs this narrow observation to correlate router conntrack state
  # with what the relay saw. It intentionally omits the transport connection
  # pid and all session state.
  defp relay_endpoint(agent) do
    identity = Agent.info(agent).public_key

    case Arc.Net.relay_endpoint(identity) do
      {:ok, %{local: {local_ip, local_port}, observed: %{"host" => host, "port" => port}}} ->
        %{
          "available" => true,
          "local" => %{"host" => Direct.Policy.format_ip(local_ip), "port" => local_port},
          "observed" => %{"host" => host, "port" => port}
        }

      _ ->
        %{"available" => false}
    end
  end

  defp direct_status(agent, address) do
    case Protocol.parse(address) do
      {:ok, target} -> route_summary(agent, target)
      _ -> %{"active" => false, "error" => "invalid_address"}
    end
  end

  defp route_summary(agent, target \\ nil) do
    manager_route_summary(Agent.direct(agent), target)
  end

  defp manager_route_summary(nil, _target), do: %{"active" => false}

  defp manager_route_summary(manager, target) do
    if target do
      case Direct.route(manager, target) do
        %{generation: generation, deadline: deadline} ->
          status = Direct.status(manager) |> Enum.find(&(&1.generation == generation))

          %{
            "active" => is_map(status) and status.phase == :active,
            "role" => if(status, do: Atom.to_string(status.role), else: nil),
            "selected" => selected_direction(manager, generation),
            "remaining_ms" => max(0, deadline - System.monotonic_time(:millisecond))
          }

        _ ->
          %{"active" => false}
      end
    else
      case Direct.status(manager) |> List.first() do
        %{generation: generation, phase: phase, role: role, remaining_ms: remaining_ms} ->
          %{
            "active" => phase == :active,
            "role" => Atom.to_string(role),
            "selected" => selected_direction(manager, generation),
            "remaining_ms" => remaining_ms
          }

        _ ->
          %{"active" => false}
      end
    end
  end

  # selected is private manager state today.  This extracts only its public
  # direction marker; no session, certificate, key, candidate, or packet is
  # emitted.
  defp selected_direction(manager, generation) do
    case :sys.get_state(manager) do
      %{routes: %{^generation => %{selected: direction}}} when is_binary(direction) -> direction
      _ -> nil
    end
  end

  # A SQLite response is JSON and the lab uses synthetic rows, so retaining a
  # bounded decoded response lets the orchestrator assert read/write results.
  # Opaque replies, including binary-echo responses that might mirror request
  # data, are represented by their length alone.
  defp response_summary(body) when byte_size(body) <= 65_536 do
    result = %{"ok" => true, "reply_bytes" => byte_size(body)}

    case decode_json(body) do
      {:ok, decoded} -> Map.put(result, "body_json", decoded)
      :error -> result
    end
  end

  defp response_summary(body), do: %{"ok" => true, "reply_bytes" => byte_size(body)}

  defp decode_json(body) do
    {:ok, :json.decode(body)}
  rescue
    _ -> :error
  end

  defp configure_state(state_root) do
    root = Path.expand(state_root)
    Enum.each(@identity_envs, &System.delete_env/1)
    Application.put_env(:arc_identity, :keys_dir, Path.join(root, "keys"))
    Application.put_env(:arc_identity, :default_file, Path.join(root, "default_key"))
    Application.put_env(:arc_control, :control_dir, Path.join(root, "control"))
    Application.put_env(:arc_data, :mailbox_dir, Path.join(root, "mailbox"))
    Application.put_env(:arc_cli, :tool_registry_dir, Path.join(root, "tools"))
    Application.put_env(:arc_cli, :trust_store_dir, Path.join(root, "trust"))
  end

  defp load_config(path, required) do
    config = path |> File.read!() |> :json.decode()

    if is_map(config) and Enum.all?(required, &Map.has_key?(config, &1)) do
      config
    else
      raise "invalid runner configuration"
    end
  rescue
    _ -> raise "invalid runner configuration"
  end

  defp relay_config!(%{"host" => host, "port" => port, "public_key" => key}) do
    {string!(host, "relay.host"), integer!(port, "relay.port", 1, 65_535),
     hex_key!(key, "relay.public_key")}
  end

  defp relay_config!(_), do: raise("invalid relay configuration")

  defp hex_key!(value, label) do
    case value do
      key when is_binary(key) and byte_size(key) == 64 ->
        case Base.decode16(key, case: :mixed) do
          {:ok, <<_::binary-size(32)>> = decoded} -> decoded
          _ -> raise "invalid #{label}"
        end

      _ ->
        raise "invalid #{label}"
    end
  end

  defp string!(value, _label) when is_binary(value) and value != "", do: value
  defp string!(_value, label), do: raise("invalid #{label}")

  defp integer!(value, _label, low, high)
       when is_integer(value) and value >= low and value <= high,
       do: value

  defp integer!(_value, label, _low, _high), do: raise("invalid #{label}")

  defp command(line) do
    case :json.decode(String.trim(line)) do
      command when is_map(command) -> command
      _ -> %{}
    end
  rescue
    _ -> %{}
  end

  defp emit(payload), do: IO.puts([@prefix, :json.encode(payload)])
  defp safe_reason(reason) when is_atom(reason), do: Atom.to_string(reason)
  defp safe_reason(_reason), do: "request_failed"
  defp safe_error(error), do: error |> Exception.message() |> String.slice(0, 240)

  defp usage do
    IO.puts(
      :stderr,
      "usage: runner.exs initialize STATE_ROOT | relay CONFIG | provider CONFIG | citizen CONFIG"
    )

    System.halt(64)
  end
end

Arc.TraversalLab.Runner.main(System.argv())
