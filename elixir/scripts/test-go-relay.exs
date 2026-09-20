# Runs Elixir clients against the Go relay.
#
#     mise run go.relay
#
# The script builds the Go relay, starts it on a free port, and then connects
# two Elixir agents to it. The agents announce, resolve, and exchange one
# message. This proves that the Go relay speaks the wire format of today.

alias Arc.Data.Agent
alias Arc.Identity

root = File.cwd!()
work = Path.join(System.tmp_dir!(), "arc-go-relay-#{:erlang.unique_integer([:positive])}")
File.mkdir_p!(work)

binary = Path.join(work, "arc-relay")
IO.puts("building #{binary}")

{_output, 0} =
  System.cmd("go", ["build", "-o", binary, "./cmd/arc-relay"],
    cd: Path.expand("..", root),
    into: IO.stream(:stdio, :line)
  )

port =
  Port.open({:spawn_executable, binary}, [
    :binary,
    :exit_status,
    {:line, 4096},
    {:args, ["--address", "127.0.0.1:0", "--store", work, "--key", "relay", "--generate"]}
  ])

{relay_key, relay_port} =
  receive do
    {^port, {:data, {:eol, line}}} ->
      ["relay", _name, key, "on", address] = String.split(line, " ", trim: true)
      [_host, relay_port] = String.split(address, ":")
      {Base.decode16!(key, case: :lower), String.to_integer(relay_port)}
  after
    30_000 -> raise "the Go relay did not start"
  end

IO.puts("the Go relay listens on 127.0.0.1:#{relay_port}")

# Closing the port shuts the pipe, and leaves the relay running. The script
# therefore stops the relay by its operating system process id.
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

alice = Identity.generate()
bob = Identity.generate()

{:ok, alice_agent} = Agent.start_link(alice)
{:ok, bob_agent} = Agent.start_link(bob)
:ok = Agent.publish(alice_agent)
:ok = Agent.publish(bob_agent)

:ok = Arc.Net.connect_relay(~c"127.0.0.1", relay_port, alice, relay_key)
:ok = Arc.Net.connect_relay(~c"127.0.0.1", relay_port, bob, relay_key)
check.(true, "two Elixir clients joined the Go relay")

:ok = Agent.publish_relay(bob_agent)
check.(true, "the Go relay took an Elixir announcement")

{:ok, found} = Agent.connect(alice_agent, Identity.short_name(bob))
check.(found.public_key == bob.public_key, "the Go relay resolved a petname")

:ok = Agent.send_message(alice_agent, Base.encode16(bob.public_key, case: :lower), "over the Go relay")

inbox =
  Enum.reduce_while(1..100, [], fn _try, _acc ->
    case Agent.read_inbox(bob_agent) do
      [] ->
        Process.sleep(100)
        {:cont, []}

      messages ->
        {:halt, messages}
    end
  end)

check.(inbox != [], "the Go relay carried a message between two Elixir citizens")

body = inbox |> List.first() |> Map.get(:text)
check.(body == "over the Go relay", "the message arrived whole: #{inspect(body)}")

IO.puts("\nthe Go relay answers the Elixir implementation")
stop.(0)
