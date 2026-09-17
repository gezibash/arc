#!/usr/bin/env elixir

# Starts one isolated ARC CLI process. Protocol integration tests can share
# only the local control and mailbox directories between otherwise separate
# citizens and providers, which exercises the explicit `--local` path without
# sharing identities or key material.

[_script | [state_root | argv]] = System.argv()

state_root = Path.expand(state_root)

for name <- ["ARC_KEY", "ARC_RELAY", "ARC_RELAY_PUBKEY", "ARC_RELAY_KEY", "ARC_RELAY_PORT"] do
  System.delete_env(name)
end

shared_control = System.get_env("ARC_TEST_CONTROL") || Path.join(state_root, "control")
shared_mailbox = System.get_env("ARC_TEST_MAILBOX") || Path.join(state_root, "mailbox")

Application.put_env(:arc_identity, :keys_dir, Path.join(state_root, "keys"))
Application.put_env(:arc_identity, :default_file, Path.join(state_root, "default_key"))
Application.put_env(:arc_control, :control_dir, shared_control)
Application.put_env(:arc_data, :mailbox_dir, shared_mailbox)
Application.put_env(:arc_net, :relay_config_path, Path.join(state_root, "relays.json"))
Application.put_env(:arc_cli, :tool_registry_dir, Path.join(state_root, "tools"))
Application.put_env(:arc_cli, :trust_store_dir, Path.join(state_root, "trust"))
Application.put_env(:arc_cli, :exit_mode, :halt)

if match?([command | _] when command in ["relay", "serve"], argv) do
  {:ok, _task} = Task.start(fn -> Arc.CLI.main(argv) end)

  case IO.gets("") do
    "shutdown\n" -> System.stop(0)
    :eof -> System.stop(0)
    _ -> System.stop(1)
  end
else
  Arc.CLI.main(argv)
end
