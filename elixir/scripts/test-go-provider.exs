# Runs the Go exec provider under the Elixir runtime.
#
#     mise run go.provider
#
# The script builds the Go provider, starts a relay, starts an Elixir agent
# that serves the Go binary, and runs the Go test that calls it.

root = File.cwd!()
work = Path.join(System.tmp_dir!(), "arc-go-provider-#{:erlang.unique_integer([:positive])}")
File.mkdir_p!(Path.join(work, "jobs"))

binary = Path.join(work, "exec-provider")
IO.puts("building #{binary}")

{_output, 0} =
  System.cmd("go", ["build", "-o", binary, "./cmd/exec-provider"],
    cd: Path.expand("..", root),
    into: IO.stream(:stdio, :line)
  )

caller = Arc.Identity.generate()
provider = Arc.Identity.generate()
relay_identity = Arc.Identity.generate()

config_path = Path.join(work, "exec.json")

File.write!(
  config_path,
  :json.encode(%{
    "grants" => [Arc.Identity.encode_public_key(caller)],
    "cwd" => work,
    "jobs_dir" => Path.join(work, "jobs")
  })
)

System.put_env("EXEC_CONFIG", config_path)

{:ok, relay} = Arc.Net.Relay.start_link(0, relay_identity: relay_identity)
port = Arc.Net.Relay.get_port(relay)

manifest = Path.expand("../cmd/exec-provider/manifest.json", root)
uri = "exec://#{binary}?manifest=#{URI.encode_www_form(manifest)}"

{:ok, agent} = Arc.Data.Agent.start_link(provider, serve: uri)
:ok = Arc.Data.Agent.publish(agent)
:ok = Arc.Net.connect_relay(~c"127.0.0.1", port, provider, relay_identity.public_key)
:ok = Arc.Data.Agent.publish_relay(agent)

IO.puts("provider #{Arc.Identity.name(provider)} serves the Go binary on port #{port}")

{_output, status} =
  System.cmd(
    "go",
    ["test", "-count=1", "-v", "-run", "GoProvider", "./client/"],
    cd: Path.expand("..", root),
    env: [
      {"ARC_RELAY_ADDRESS", "127.0.0.1:#{port}"},
      {"ARC_RELAY_KEY", Arc.Identity.encode_public_key(relay_identity)},
      {"ARC_PROVIDER_KEY", Arc.Identity.encode_public_key(provider)},
      {"ARC_CALLER_SEED", Base.encode16(caller.seed, case: :lower)}
    ],
    into: IO.stream(:stdio, :line)
  )

File.rm_rf(work)
System.halt(status)
