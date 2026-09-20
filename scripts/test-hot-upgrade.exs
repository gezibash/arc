#!/usr/bin/env elixir

# This is an isolated release-handler proof. It deliberately creates all
# application directories beneath one temporary directory and starts a fresh
# VM with `mix run --no-start`. It does not touch a deployed ARC release.

defmodule Arc.HotUpgrade.Proof do
  alias Arc.Data.{Packet, Session}
  alias Arc.Identity
  alias Arc.Net.{Handshake, Relay}

  @app :arc_net
  @old_vsn "0.0.0-proof.1"
  @new_vsn "0.0.0-proof.2"

  def run do
    if Enum.any?(Application.started_applications(), fn {app, _, _} ->
         String.starts_with?(Atom.to_string(app), "arc_")
       end) do
      raise "run this proof in a fresh VM with mix run --no-start"
    end

    root = make_root()

    try do
      setup_release_dirs(root)
      supervisor = start_upgradeable_relay(root)
      verify_upgrade_and_downgrade(root, supervisor)
      IO.puts("hot-upgrade proof: PASS")
    after
      Application.delete_env(:arc_net, :hot_upgrade_proof_owner)
      stop_apps()
      {:ok, _removed} = File.rm_rf(root)
    end
  end

  defp make_root do
    root =
      Path.join(
        System.tmp_dir!(),
        "arc-hot-upgrade-" <> Base.encode16(:crypto.strong_rand_bytes(16), case: :lower)
      )

    case File.mkdir(root) do
      :ok -> root
      {:error, :eexist} -> make_root()
      {:error, reason} -> raise "could not create proof directory: #{inspect(reason)}"
    end
  end

  defp setup_release_dirs(root) do
    old_dir = Path.join(root, "arc_net-#{@old_vsn}")
    new_dir = Path.join(root, "arc_net-#{@new_vsn}")
    old_ebin = Path.join(old_dir, "ebin")
    new_ebin = Path.join(new_dir, "ebin")
    File.mkdir_p!(old_ebin)
    File.mkdir_p!(new_ebin)

    source_ebin = @app |> :code.lib_dir() |> List.to_string() |> Path.join("ebin")
    copy_ebin(source_ebin, old_ebin)
    copy_ebin(source_ebin, new_ebin)
    write_app(Path.join(old_ebin, "arc_net.app"), @old_vsn)
    write_app(Path.join(new_ebin, "arc_net.app"), @new_vsn)
    write_fixture_application_beam(old_ebin)
    write_fixture_application_beam(new_ebin)
    write_fixture_beam(old_ebin, :old)
    write_fixture_beam(new_ebin, :new)
    write_appup(Path.join(new_ebin, "arc_net.appup"))

    # Mix has loaded the checkout's application specification even with
    # --no-start. Unload it before placing the synthetic old version first.
    :ok = :application.unload(@app)

    # The old application must be first when ARC starts. release_handler later
    # changes the path itself as part of the appup evaluation.
    :code.del_path(String.to_charlist(source_ebin))
    true = :code.add_patha(String.to_charlist(old_ebin))
  end

  defp copy_ebin(source, destination) do
    source
    |> File.ls!()
    |> Enum.each(fn name ->
      source_path = Path.join(source, name)

      if File.regular?(source_path) do
        File.cp!(source_path, Path.join(destination, name))
      end
    end)
  end

  defp write_app(path, version) do
    {:ok, [{:application, @app, properties}]} = :file.consult(String.to_charlist(path))

    properties =
      properties
      |> Keyword.put(:vsn, String.to_charlist(version))
      |> Keyword.put(:mod, {Arc.HotUpgrade.FixtureApplication, []})

    File.write!(path, :io_lib.format("~p.~n", [{:application, @app, properties}]))
  end

  defp write_appup(path) do
    # `advanced` forces sys:change_code/4 and Relay.code_change/3. The same
    # reversible instruction is used for both directions.
    relay = Arc.Net.Relay

    term =
      {String.to_charlist(@new_vsn),
       [
         {String.to_charlist(@old_vsn),
          [{:update, relay, {:advanced, []}, :soft_purge, :soft_purge, []}]}
       ],
       [
         {String.to_charlist(@old_vsn),
          [{:update, relay, {:advanced, []}, :soft_purge, :soft_purge, []}]}
       ]}

    File.write!(path, :io_lib.format("~p.~n", [term]))
  end

  defp write_fixture_beam(ebin, version) do
    source_path = Path.expand("../apps/arc_net/lib/arc/net/relay.ex", __DIR__)

    upgraded_source =
      source_path |> File.read!() |> upgrade_relay_ast(version) |> Macro.to_string()

    generated_source = Path.join(Path.dirname(ebin), "relay_hot_#{version}.ex")
    compile_fixture_source(ebin, generated_source, upgraded_source)
  end

  defp write_fixture_application_beam(ebin) do
    source = """
    defmodule Arc.HotUpgrade.FixtureApplication do
      use Application

      @impl true
      def start(_type, _args) do
        children = [
          {Task.Supervisor, name: Arc.Net.TaskSupervisor},
          {Registry, keys: :unique, name: Arc.Net.TransportRegistry},
          {Registry, keys: :unique, name: Arc.Net.DirectRegistry},
          {DynamicSupervisor, strategy: :one_for_one, name: Arc.Net.TransportSupervisor},
          Arc.Net.TransportManager,
          {Arc.Net.Relay, 0}
        ]

        Supervisor.start_link(children, strategy: :one_for_one, name: Arc.Net.Supervisor)
      end
    end
    """

    compile_fixture_source(ebin, Path.join(Path.dirname(ebin), "fixture_application.ex"), source)
  end

  defp compile_fixture_source(ebin, generated_source, source) do
    File.write!(generated_source, source)

    elixirc = System.find_executable("elixirc") || raise "elixirc is unavailable"

    paths =
      [:arc_net, :arc_data, :arc_identity]
      |> Enum.map(fn app ->
        ["-pa", app |> :code.lib_dir() |> List.to_string() |> Path.join("ebin")]
      end)
      |> List.flatten()

    {output, status} =
      System.cmd(elixirc, paths ++ ["-o", ebin, generated_source], stderr_to_stdout: true)

    if status != 0 do
      raise "could not compile fixture beam:\n#{output}"
    end
  end

  # Both synthetic beams are mechanically derived from ARC's real Relay source.
  # They append only the reversible proof migration and its observation hook.
  defp upgrade_relay_ast(source, version) do
    {:defmodule, meta, [name, [do: body]]} = Code.string_to_quoted!(source, columns: true)

    additions =
      fixture_additions(version)

    {:defmodule, meta, [name, [do: append_to_block(body, additions)]]}
  end

  defp fixture_additions(:new) do
    quote do
      @doc false
      def hot_upgrade_marker(relay_pid), do: GenServer.call(relay_pid, :hot_upgrade_marker)

      @impl GenServer
      def code_change({:down, _}, state, _extra) do
        hot_upgrade_proof_pause(:downgrade)
        {:ok, Map.delete(state, :hot_upgrade_marker)}
      end

      def code_change(_old_vsn, state, _extra) do
        hot_upgrade_proof_pause(:upgrade)
        {:ok, Map.put(state, :hot_upgrade_marker, :v2)}
      end

      @impl GenServer
      def handle_call(:hot_upgrade_marker, _from, state) do
        {:reply, Map.get(state, :hot_upgrade_marker), state}
      end

      defp hot_upgrade_proof_pause(phase) do
        case Application.get_env(:arc_net, :hot_upgrade_proof_owner) do
          owner when is_pid(owner) ->
            send(owner, {:hot_upgrade_state_paused, phase, self()})

            receive do
              {:continue_hot_upgrade, ^phase} -> :ok
            after
              2_000 -> raise "hot-upgrade proof did not release #{phase}"
            end

          _ ->
            :ok
        end
      end
    end
  end

  defp fixture_additions(:old) do
    quote do
      @impl GenServer
      def code_change({:down, _}, state, _extra) do
        case Application.get_env(:arc_net, :hot_upgrade_proof_owner) do
          owner when is_pid(owner) ->
            send(owner, {:hot_upgrade_state_paused, :downgrade, self()})

            receive do
              {:continue_hot_upgrade, :downgrade} -> :ok
            after
              2_000 -> raise "hot-upgrade proof did not release downgrade"
            end

          _ ->
            :ok
        end

        {:ok, Map.delete(state, :hot_upgrade_marker)}
      end

      def code_change(_old_vsn, state, _extra), do: {:ok, state}
    end
  end

  defp append_to_block({:__block__, meta, expressions}, additions),
    do: {:__block__, meta, expressions ++ block_expressions(additions)}

  defp append_to_block(expression, additions),
    do: {:__block__, [], [expression | block_expressions(additions)]}

  defp block_expressions({:__block__, _, expressions}), do: expressions
  defp block_expressions(expression), do: [expression]

  defp start_upgradeable_relay(root) do
    Application.put_env(:arc_control, :control_dir, Path.join(root, "control"))
    Application.put_env(:arc_identity, :keys_dir, Path.join(root, "keys"))
    Application.put_env(:arc_identity, :default_file, Path.join(root, "default.key"))

    sasl_ebin = :release_handler |> :code.which() |> List.to_string() |> Path.dirname()
    true = :code.add_patha(String.to_charlist(sasl_ebin))
    {:ok, _} = Application.ensure_all_started(:sasl)
    {:ok, _} = Application.ensure_all_started(@app)
    loaded_version = @app |> Application.spec(:vsn) |> to_string()

    if loaded_version != @old_vsn do
      raise "expected synthetic #{@old_vsn}, got #{loaded_version}"
    end

    relay = Process.whereis(Relay) || raise "fixture relay did not start"
    relay
  end

  defp verify_upgrade_and_downgrade(root, relay) do
    port = Relay.get_port(relay)

    alice = Identity.generate()
    bob = Identity.generate()
    alice_socket = relay_connect(port, alice)
    bob_socket = relay_connect(port, bob)

    try do
      wait_for_routes(relay, alice.public_key, bob.public_key)
      before = snapshot(relay, alice.public_key, bob.public_key)
      channels = establish_channels(alice, bob)

      channels =
        assert_bidirectional(alice_socket, bob_socket, alice, bob, channels, "before-upgrade")

      Application.put_env(:arc_net, :hot_upgrade_proof_owner, self())

      new_dir = Path.join(root, "arc_net-#{@new_vsn}")

      channels =
        run_transition(
          :upgrade,
          fn -> :release_handler.upgrade_app(@app, String.to_charlist(new_dir)) end,
          fn ->
            assert_bidirectional(alice_socket, bob_socket, alice, bob, channels, "during-upgrade")
          end
        )

      assert_same_runtime(before, relay, alice.public_key, bob.public_key)
      true = to_string(Application.spec(@app, :vsn)) == @new_vsn
      :v2 = apply(Relay, :hot_upgrade_marker, [relay])

      channels =
        assert_bidirectional(alice_socket, bob_socket, alice, bob, channels, "after-upgrade")

      Application.put_env(:arc_net, :hot_upgrade_proof_owner, self())

      old_dir = Path.join(root, "arc_net-#{@old_vsn}")

      {:ok, downgrade_script} =
        :release_handler.downgrade_script(
          @app,
          String.to_charlist(@old_vsn),
          String.to_charlist(old_dir)
        )

      unless Enum.any?(downgrade_script, &match?({:code_change, :down, _}, &1)) do
        raise "downgrade appup did not produce an advanced code-change instruction"
      end

      channels =
        run_transition(
          :downgrade,
          fn ->
            :release_handler.downgrade_app(
              @app,
              String.to_charlist(@old_vsn),
              String.to_charlist(old_dir)
            )
          end,
          fn ->
            assert_bidirectional(
              alice_socket,
              bob_socket,
              alice,
              bob,
              channels,
              "during-downgrade"
            )
          end
        )

      assert_same_runtime(before, relay, alice.public_key, bob.public_key)
      true = to_string(Application.spec(@app, :vsn)) == @old_vsn
      false = Map.has_key?(:sys.get_state(relay), :hot_upgrade_marker)
      false = :erlang.function_exported(Relay, :hot_upgrade_marker, 1)

      _channels =
        assert_bidirectional(alice_socket, bob_socket, alice, bob, channels, "after-downgrade")

      IO.puts("relay pid: #{inspect(relay)}")
      IO.puts("client connection pids: #{inspect(before.connections)}")
      IO.puts("listener port: #{before.port}")
      IO.puts("messages: before/during/after upgrade and downgrade")
    after
      :gen_tcp.close(alice_socket)
      :gen_tcp.close(bob_socket)
    end
  end

  defp snapshot(relay, alice_key, bob_key) do
    state = :sys.get_state(relay)

    %{
      relay: relay,
      port: Relay.get_port(relay),
      socket: state.listen_socket,
      route_tables: state.routes_tables,
      acceptors: state.acceptor_refs |> Map.keys() |> Enum.sort(),
      shards: state.shard_pids |> Map.values() |> Enum.sort(),
      connections: %{
        alice: Relay.route_for(relay, alice_key),
        bob: Relay.route_for(relay, bob_key)
      }
    }
  end

  defp assert_same_runtime(before, relay, alice_key, bob_key) do
    after_snapshot = snapshot(relay, alice_key, bob_key)
    port = before.port
    socket = before.socket
    route_tables = before.route_tables
    acceptors = before.acceptors
    shards = before.shards
    connections = before.connections

    true = Process.alive?(relay)
    ^relay = after_snapshot.relay
    ^port = after_snapshot.port
    ^socket = after_snapshot.socket
    ^route_tables = after_snapshot.route_tables
    ^acceptors = after_snapshot.acceptors
    ^shards = after_snapshot.shards
    ^connections = after_snapshot.connections
    Enum.each(acceptors, &assert_alive/1)
    Enum.each(shards, &assert_alive/1)
    Enum.each(Map.values(connections), &assert_alive/1)
  end

  defp assert_alive(pid) when is_pid(pid) do
    true = Process.alive?(pid)
  end

  defp run_transition(phase, operation, traffic) do
    task = Task.async(operation)

    channels =
      receive do
        {:hot_upgrade_state_paused, ^phase, relay} when is_pid(relay) ->
          channels = traffic.()
          send(relay, {:continue_hot_upgrade, phase})
          channels
      after
        2_500 ->
          result = Task.yield(task, 100) || :still_running
          raise("#{phase} never reached Relay.code_change/3: #{inspect(result)}")
      end

    expect_ok(Task.await(task, 3_000), Atom.to_string(phase))
    channels
  end

  defp relay_connect(port, identity) do
    {:ok, socket} = :gen_tcp.connect(~c"127.0.0.1", port, [:binary, packet: :raw, active: false])
    {:ok, relay_hello} = :gen_tcp.recv(socket, 64, 2_000)
    {:ok, relay_key, challenge} = Handshake.decode_relay_hello(relay_hello)
    {:ok, <<info_length::32-big>>} = :gen_tcp.recv(socket, 4, 2_000)
    {:ok, _info} = :gen_tcp.recv(socket, info_length, 2_000)
    {:ok, hello, _} = Handshake.client_hello(identity, relay_key, challenge)
    :ok = :gen_tcp.send(socket, hello)
    socket
  end

  defp wait_for_routes(relay, alice_key, bob_key, attempts \\ 40)

  defp wait_for_routes(_relay, _alice_key, _bob_key, 0),
    do: raise("relay routes did not authenticate")

  defp wait_for_routes(relay, alice_key, bob_key, attempts) do
    if is_pid(Relay.route_for(relay, alice_key)) and is_pid(Relay.route_for(relay, bob_key)) do
      :ok
    else
      Process.sleep(25)
      wait_for_routes(relay, alice_key, bob_key, attempts - 1)
    end
  end

  defp establish_channels(alice, bob) do
    {:ok, alice_to_bob} = Session.establish(alice, bob.public_key)
    {:ok, bob_to_alice} = Session.establish(bob, alice.public_key)
    %{alice_to_bob: alice_to_bob, bob_to_alice: bob_to_alice}
  end

  defp assert_bidirectional(alice_socket, bob_socket, alice, bob, channels, phase) do
    alice_to_bob =
      send_and_receive(
        alice_socket,
        bob_socket,
        alice,
        bob,
        channels.alice_to_bob,
        "#{phase}:alice-to-bob"
      )

    bob_to_alice =
      send_and_receive(
        bob_socket,
        alice_socket,
        bob,
        alice,
        channels.bob_to_alice,
        "#{phase}:bob-to-alice"
      )

    %{alice_to_bob: alice_to_bob, bob_to_alice: bob_to_alice}
  end

  defp send_and_receive(sender_socket, receiver_socket, sender, receiver, session, tag_prefix) do
    tag = "#{tag_prefix}:#{session.seq}"
    {nonce, ciphertext, sequence, session} = Session.encrypt(session, tag)

    packet =
      Packet.encode(sender, receiver.public_key, session.session_id, sequence, nonce, ciphertext,
        ek: session.ek_pub
      )

    :ok = :gen_tcp.send(sender_socket, <<byte_size(packet)::32-big, packet::binary>>)
    {:ok, <<length::32-big>>} = :gen_tcp.recv(receiver_socket, 4, 2_000)
    {:ok, received} = :gen_tcp.recv(receiver_socket, length, 2_000)
    {:ok, decoded} = Packet.decode(received)
    receiver_key = receiver.public_key
    sender_key = sender.public_key
    ^receiver_key = decoded.dst
    ^sender_key = decoded.src
    ^sequence = decoded.seq
    session_id = session.session_id
    ephemeral_key = session.ek_pub
    ^session_id = decoded.session_id
    ^ephemeral_key = decoded.ek

    responder_session =
      Session.accept(receiver, sender.public_key, decoded.ek, decoded.session_id)

    {:ok, ^tag} = Session.decrypt(responder_session, decoded.nonce, decoded.ciphertext)
    session
  end

  defp expect_ok({:ok, []}, _operation), do: :ok

  defp expect_ok(result, operation),
    do: raise("#{operation} did not complete cleanly: #{inspect(result)}")

  defp stop_apps do
    _ = Application.stop(@app)
    _ = Application.stop(:sasl)
    Application.delete_env(:arc_control, :control_dir)
    Application.delete_env(:arc_identity, :keys_dir)
    Application.delete_env(:arc_identity, :default_file)
    :ok
  end
end

Arc.HotUpgrade.Proof.run()
