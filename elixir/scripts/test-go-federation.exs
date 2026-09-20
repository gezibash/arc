# Federates a Go relay with an Elixir relay.
#
#     mise run go.federation
#
# The script starts one relay of each implementation, links them, serves an
# Elixir citizen that shares with the network, and calls it from a Go client
# on the other relay.

alias Arc.Data.Agent
alias Arc.Identity

root = File.cwd!()
work = Path.join(System.tmp_dir!(), "arc-go-federation-#{:erlang.unique_integer([:positive])}")
File.mkdir_p!(Path.join(work, "keys"))

binary = Path.join(work, "arc-relay")
IO.puts("building #{binary}")

{_output, 0} =
  System.cmd("go", ["build", "-o", binary, "./cmd/arc-relay"],
    cd: Path.expand("..", root),
    into: IO.stream(:stdio, :line)
  )

# The Go relay needs its identity before it starts, because the Elixir relay
# names its partner at start as well.
go_identity = Identity.generate()

File.write!(
  Path.join([work, "keys", "#{Identity.name(go_identity)}.toml"]),
  "[identity]\nseed = \"#{Base.encode16(go_identity.seed, case: :lower)}\"\n"
)

# A free port for the Go relay.
{:ok, socket} = :gen_tcp.listen(0, [:binary, ip: {127, 0, 0, 1}])
{:ok, go_port} = :inet.port(socket)
:ok = :gen_tcp.close(socket)

elixir_identity = Identity.generate()

{:ok, relay} =
  Arc.Net.Relay.start_link(0,
    relay_identity: elixir_identity,
    federation_transit: true,
    federation_peers: [%{public_key: go_identity.public_key, host: "127.0.0.1", port: go_port}]
  )

elixir_port = Arc.Net.Relay.get_port(relay)

port =
  Port.open({:spawn_executable, binary}, [
    :binary,
    :exit_status,
    {:line, 4096},
    {:args,
     [
       "--address",
       "127.0.0.1:#{go_port}",
       "--store",
       work,
       "--key",
       Identity.name(go_identity),
       "--transit",
       "--peer",
       "#{Identity.encode_public_key(elixir_identity)}@127.0.0.1:#{elixir_port}"
     ]}
  ])

receive do
  {^port, {:data, {:eol, line}}} -> IO.puts("go relay: #{line}")
after
  30_000 -> raise "the Go relay did not start"
end

stop = fn status ->
  case Port.info(port, :os_pid) do
    {:os_pid, os_pid} -> System.cmd("kill", [Integer.to_string(os_pid)])
    _ -> :ok
  end

  File.rm_rf(work)
  System.halt(status)
end

check = fn
  true, message -> IO.puts("ok   #{message}")
  _false, message -> IO.puts("FAIL #{message}") && stop.(1)
end

IO.puts("the Elixir relay listens on 127.0.0.1:#{elixir_port}, the Go relay on #{go_port}")

# An Elixir citizen on the Elixir relay, shared with the network.
provider = Identity.generate()
{:ok, agent} = Agent.start_link(provider)
:ok = Agent.publish(agent)
:ok = Arc.Net.connect_relay(~c"127.0.0.1", elixir_port, provider, elixir_identity.public_key)

:ok =
  Agent.publish_relay(agent,
    federation: :network,
    relay_public_key: elixir_identity.public_key
  )

check.(true, "an Elixir citizen serves on the Elixir relay")

# The Go client of the test writes with this key, so the Elixir citizen can
# answer it.
caller = Identity.generate()

# The citizen answers the first message that reaches it.
answering =
  Task.async(fn ->
    Enum.reduce_while(1..300, :waiting, fn _try, _acc ->
      case Agent.read_inbox(agent) do
        [] ->
          Process.sleep(100)
          {:cont, :waiting}

        [message | _] ->
          Agent.send_message(
            agent,
            Base.encode16(caller.public_key, case: :lower),
            "the citizen of the other relay answers"
          )

          {:halt, {:received, Map.get(message, :text)}}
      end
    end)
  end)

# The Go test calls it from the Go relay.
{_output, status} =
  System.cmd(
    "go",
    ["test", "-count=1", "-v", "-run", "AcrossTheFederation", "./client/"],
    cd: Path.expand("..", root),
    env: [
      {"ARC_RELAY_ADDRESS", "127.0.0.1:#{go_port}"},
      {"ARC_RELAY_KEY", Identity.encode_public_key(go_identity)},
      {"ARC_PROVIDER_KEY", Identity.encode_public_key(provider)},
      {"ARC_CALLER_SEED", Base.encode16(caller.seed, case: :lower)}
    ],
    into: IO.stream(:stdio, :line)
  )

received = Task.await(answering, 5_000)
check.(match?({:received, _}, received), "the Elixir citizen received the message of the Go client: #{inspect(received)}")

check.(status == 0, "a Go client reached the Elixir citizen across the two relays")

IO.puts("\nthe Go relay and the Elixir relay federate")
stop.(0)
