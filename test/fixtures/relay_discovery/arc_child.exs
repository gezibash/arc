#!/usr/bin/env elixir

# Boots an ARC CLI process with state isolated beneath the directory supplied
# by the integration test.  It deliberately sets configuration before
# Arc.CLI starts its supervised applications.

# `mix run path/to/script.exs -- ...` leaves the script path in argv.
[_script | [state_root | argv]] = System.argv()

state_root = Path.expand(state_root)

# Do not inherit a developer's ARC identity or relay selection. Every test
# child uses its own generated default identity and explicit CLI relay flags.
System.delete_env("ARC_KEY")
System.delete_env("ARC_RELAY")
System.delete_env("ARC_RELAY_PUBKEY")

Application.put_env(:arc_identity, :keys_dir, Path.join(state_root, "keys"))
Application.put_env(:arc_identity, :default_file, Path.join(state_root, "default_key"))
Application.put_env(:arc_control, :control_dir, Path.join(state_root, "control"))
Application.put_env(:arc_data, :mailbox_dir, Path.join(state_root, "mailbox"))
Application.put_env(:arc_cli, :tool_registry_dir, Path.join(state_root, "tools"))
Application.put_env(:arc_cli, :trust_store_dir, Path.join(state_root, "trust"))
Application.put_env(:arc_cli, :exit_mode, :halt)

if match?(["serve" | _], argv) do
  {:ok, _server} = Task.start(fn -> Arc.CLI.main(argv) end)

  case IO.gets("") do
    "shutdown\n" -> System.stop(0)
    :eof -> System.stop(0)
    _ -> System.stop(1)
  end
else
  Arc.CLI.main(argv)
end
