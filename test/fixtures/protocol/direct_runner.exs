#!/usr/bin/env elixir

# Runs one relay-backed protocol request while keeping the agent alive long
# enough to report the promoted route. The report is deliberately limited to
# public route state; it never prints identities, private keys, or carrier
# credentials.

[_script | [state_root, policy_path, address, relay_host, relay_port, relay_key, body]] =
  System.argv()

state_root = Path.expand(state_root)

for name <- ["ARC_KEY", "ARC_RELAY", "ARC_RELAY_PUBKEY", "ARC_RELAY_KEY", "ARC_RELAY_PORT"] do
  System.delete_env(name)
end

Application.put_env(:arc_identity, :keys_dir, Path.join(state_root, "keys"))
Application.put_env(:arc_identity, :default_file, Path.join(state_root, "default_key"))
Application.put_env(:arc_control, :control_dir, Path.join(state_root, "control"))
Application.put_env(:arc_data, :mailbox_dir, Path.join(state_root, "mailbox"))
Application.put_env(:arc_cli, :tool_registry_dir, Path.join(state_root, "tools"))
Application.put_env(:arc_cli, :trust_store_dir, Path.join(state_root, "trust"))

Application.ensure_all_started(:arc_data)
Application.ensure_all_started(:arc_net)

{:ok, rules} = Arc.Data.Direct.Policy.load_file(policy_path)
{:ok, identity} = Arc.Identity.KeyStore.resolve_active()
{:ok, relay_key} = Base.decode16(relay_key, case: :mixed)
{relay_port, ""} = Integer.parse(relay_port)
{:ok, agent} = Arc.Data.Agent.start_link(identity, direct_policy: rules)

try do
  :ok =
    Arc.Net.acquire_relay(String.to_charlist(relay_host), relay_port, identity, relay_key,
      owner_pid: agent
    )

  :ok = Arc.Data.Agent.publish_relay(agent)
  {:ok, target} = Arc.Data.Protocol.parse(address)
  result = Arc.Data.Protocol.request(agent, address, body, timeout_ms: 10_000)
  manager = Arc.Data.Agent.direct(agent)
  route = Arc.Data.Direct.route(manager, target)

  direct =
    case route do
      %{generation: generation, deadline: deadline} ->
        status =
          Arc.Data.Direct.status(manager)
          |> Enum.find(&(&1.generation == generation))

        selected =
          case :sys.get_state(manager) do
            %{routes: %{^generation => %{selected: direction}}} when is_binary(direction) ->
              direction

            _ ->
              nil
          end

        %{
          "active" => is_map(status) and status.phase == :active,
          "role" => if(status, do: Atom.to_string(status.role), else: nil),
          "selected" => selected,
          "remaining_ms" => max(0, deadline - System.monotonic_time(:millisecond))
        }

      _ ->
        %{"active" => false}
    end

  encoded_result =
    case result do
      {:ok, %{body: response}} -> %{"ok" => true, "body" => :json.decode(response)}
      {:error, reason} -> %{"ok" => false, "error" => inspect(reason)}
    end

  IO.puts(["ARC_DIRECT_RESULT ", :json.encode(%{"result" => encoded_result, "direct" => direct})])

  case IO.gets("") do
    "shutdown\n" -> :ok
    :eof -> :ok
    _ -> System.halt(1)
  end
after
  Arc.Net.release_relay(identity.public_key, agent)

  if Process.alive?(agent), do: GenServer.stop(agent, :normal)
end
