#!/usr/bin/env elixir

# Starts a single ARC CLI process with all state rooted in the directory passed
# by the federation integration test. Long-lived relay and provider commands
# stop only when the parent sends `shutdown` on stdin.

[_script | [state_root | argv]] = System.argv()

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
Application.put_env(:arc_cli, :exit_mode, :halt)

if match?([command | _] when command in ["relay", "serve"], argv) do
  {:ok, _task} = Task.start(fn -> Arc.CLI.main(argv) end)

  case IO.gets("") do
    "shutdown\n" ->
      if System.get_env("ARC_TEST_SHUTDOWN_DIAGNOSTICS") == "1" do
        IO.puts("shutdown command received")

        spawn(fn ->
          Process.sleep(750)

          snapshots =
            for pid <- Process.list(), reduce: [] do
              acc ->
                case Process.info(pid, [:registered_name, :current_stacktrace]) do
                  nil ->
                    acc

                  info ->
                    frames =
                      Enum.map(info[:current_stacktrace], fn {module, function, arity, _location} ->
                        {module, function, if(is_list(arity), do: length(arity), else: arity)}
                      end)

                    [{pid, info[:registered_name], frames} | acc]
                end
            end

          IO.puts("shutdown process stacks: #{inspect(snapshots, limit: :infinity)}")
        end)
      end

      System.stop(0)

    :eof ->
      System.stop(0)

    _ ->
      System.stop(1)
  end
else
  Arc.CLI.main(argv)
end
