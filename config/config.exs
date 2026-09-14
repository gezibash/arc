# This file is responsible for configuring your umbrella
# and **all applications** and their dependencies with the
# help of the Config module.
#
# Note that all applications in your umbrella share the
# same configuration and dependencies, which is why they
# all use the same configuration file. If you want different
# configurations or dependencies per app, it is best to
# move said applications out of the umbrella.
import Config

config :arc_data, allowed_clock_skew_ms: 120_000

# Tests must never touch the real ~/.config/arc. Every test run gets a
# fresh directory under the system temp dir.
if config_env() == :test do
  test_root = Path.join(System.tmp_dir!(), "arc-test-#{System.pid()}")
  config :arc_control, control_dir: Path.join(test_root, "control")

  config :arc_identity,
    keys_dir: Path.join(test_root, "keys"),
    default_file: Path.join(test_root, "default_key")

  # A CLI error returns {:exit, code} from Arc.CLI.main/1 instead of
  # halting the VM, so error paths are testable.
  config :arc_cli,
    exit_mode: :return,
    lists_dir: Path.join(test_root, "lists"),
    cache_dir: Path.join(test_root, "cache")
end

# Sample configuration:
#
#     config :logger, :default_handler,
#       level: :info
#
#     config :logger, :default_formatter,
#       format: "$date $time [$level] $metadata$message\n",
#       metadata: [:user_id]
#
