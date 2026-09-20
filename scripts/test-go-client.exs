# Runs the Go client against an Elixir relay.
#
#     mise run go.conformance
#
# The script starts a relay on a free port, gives its address and its public
# key to the Go test, and exits with the result of that test.

identity = Arc.Identity.generate()
{:ok, relay} = Arc.Net.Relay.start_link(0, relay_identity: identity)
port = Arc.Net.Relay.get_port(relay)
public_key = Arc.Identity.encode_public_key(identity)

IO.puts("relay #{Arc.Identity.name(identity)} on port #{port}")

{_output, status} =
  System.cmd(
    "go",
    ["test", "-count=1", "-run", "Elixir", "./client/"],
    cd: Path.join(File.cwd!(), "go"),
    env: [
      {"ARC_RELAY_ADDRESS", "127.0.0.1:#{port}"},
      {"ARC_RELAY_KEY", public_key}
    ],
    into: IO.stream(:stdio, :line)
  )

System.halt(status)
