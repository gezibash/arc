#!/usr/bin/env elixir

# Test-only probe for the public discovery reply shape. It uses the isolated
# default identity created by the integration test and emits only cached status
# plus public provider keys; private key material never reaches stdout.

[_script | [state_root, relay, relay_public_key, query]] = System.argv()

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

{:ok, _apps} = Application.ensure_all_started(:arc_net)
{:ok, identity} = Arc.Identity.KeyStore.default()
{host, port} = Arc.Net.relay_address_from(relay)
{:ok, relay_pin} = Base.decode16(relay_public_key, case: :mixed)
:ok = Arc.Net.connect_relay(host, port, identity, relay_pin)

result =
  case Arc.Net.discover_via_relay(identity.public_key, query) do
    {:ok, page} ->
      %{
        "cached" => Map.get(page, :cached?, false),
        "providers" => Enum.map(page.entries, &Base.encode16(&1.public_key, case: :lower))
      }

    {:error, reason} ->
      %{"error" => inspect(reason)}
  end

result |> :json.encode() |> IO.iodata_to_binary() |> IO.puts()
