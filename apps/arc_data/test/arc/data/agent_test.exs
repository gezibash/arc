defmodule Arc.Data.AgentTest do
  use ExUnit.Case, async: false

  alias Arc.Data.Agent
  alias Arc.Data.Frame
  alias Arc.Data.Packet
  alias Arc.Data.Session
  alias Arc.Identity

  setup do
    Arc.Control.Local.reset()
    :ok
  end

  describe "agent lifecycle" do
    test "start and get info" do
      id = Identity.generate()
      {:ok, agent} = Agent.start_link(id)

      info = Agent.info(agent)
      assert info.name == Identity.name(id)
      assert info.public_key == id.public_key
      assert info.sessions == 0
    end

    test "publish to control plane" do
      id = Identity.generate()
      {:ok, agent} = Agent.start_link(id)

      :ok = Agent.publish(agent)

      {:ok, [entry]} = Arc.Control.resolve(Identity.name(id))
      assert entry.public_key == id.public_key
      assert entry.x25519_public != nil
    end

    test "cannot start a second registered agent for the same identity" do
      id = Identity.generate()
      public_key = id.public_key
      {:ok, agent} = Agent.start_link(id)
      old_flag = Process.flag(:trap_exit, true)

      try do
        assert {:error, {:already_registered, ^public_key}} = Agent.start_link(id)
      after
        Process.flag(:trap_exit, old_flag)
      end

      GenServer.stop(agent, :normal)
    end

    test "cannot serve a handler without an explicit capability manifest" do
      runtime_path =
        System.tmp_dir!()
        |> Path.join("arc_missing_manifest_#{System.unique_integer([:positive])}.exs")

      File.write!(runtime_path, """
      #!/usr/bin/env elixir
      for _line <- IO.stream(:stdio, :line), do: IO.puts(~s({"reply":"ok"}))
      """)

      File.chmod!(runtime_path, 0o755)
      on_exit(fn -> File.rm(runtime_path) end)

      id = Identity.generate()
      serve_uri = "exec://#{runtime_path}"
      old_flag = Process.flag(:trap_exit, true)

      try do
        assert {:error, {:handler_init_failed, ^serve_uri, :missing_manifest}} =
                 Agent.start_link(id, serve: serve_uri)
      after
        Process.flag(:trap_exit, old_flag)
      end
    end
  end

  describe "connect and session" do
    test "two agents connect and establish session" do
      alice_id = Identity.generate()
      bob_id = Identity.generate()

      {:ok, alice} = Agent.start_link(alice_id)
      {:ok, bob} = Agent.start_link(bob_id)

      :ok = Agent.publish(alice)
      :ok = Agent.publish(bob)

      {:ok, _entry} = Agent.connect(alice, Identity.name(bob_id))
      {:ok, _entry} = Agent.connect(bob, Identity.name(alice_id))

      assert Agent.info(alice).sessions == 1
      assert Agent.info(bob).sessions == 1
    end

    test "connect to unknown peer returns error" do
      id = Identity.generate()
      {:ok, agent} = Agent.start_link(id)

      assert {:error, :not_found} = Agent.connect(agent, "nobody-here-00000000")
    end

    test "connect to peer without keyex returns error" do
      alice_id = Identity.generate()
      bob_id = Identity.generate()

      {:ok, alice} = Agent.start_link(alice_id)

      # Publish bob directly to control plane without keyex
      Arc.Control.publish(bob_id)

      assert {:error, :no_keyex} = Agent.connect(alice, Identity.name(bob_id))
    end
  end

  describe "encrypted messaging" do
    test "alice sends encrypted message to bob" do
      alice_id = Identity.generate()
      bob_id = Identity.generate()

      {:ok, alice} = Agent.start_link(alice_id)
      {:ok, bob} = Agent.start_link(bob_id)

      :ok = Agent.publish(alice)
      :ok = Agent.publish(bob)

      {:ok, _} = Agent.connect(alice, Identity.name(bob_id))
      {:ok, _} = Agent.connect(bob, Identity.name(alice_id))

      :ok = Agent.send_message(alice, Identity.name(bob_id), "hello bob!")

      # Give the message time to arrive
      Process.sleep(10)

      messages = Agent.read_inbox(bob)
      assert length(messages) == 1
      [msg] = messages
      assert msg.text == "hello bob!"
      assert msg.from == Identity.name(alice_id)
    end

    test "bidirectional messaging" do
      alice_id = Identity.generate()
      bob_id = Identity.generate()

      {:ok, alice} = Agent.start_link(alice_id)
      {:ok, bob} = Agent.start_link(bob_id)

      :ok = Agent.publish(alice)
      :ok = Agent.publish(bob)

      {:ok, _} = Agent.connect(alice, Identity.name(bob_id))
      {:ok, _} = Agent.connect(bob, Identity.name(alice_id))

      :ok = Agent.send_message(alice, Identity.name(bob_id), "hey bob")
      :ok = Agent.send_message(bob, Identity.name(alice_id), "hey alice")

      Process.sleep(10)

      bob_msgs = Agent.read_inbox(bob)
      alice_msgs = Agent.read_inbox(alice)

      assert length(bob_msgs) == 1
      assert length(alice_msgs) == 1
      assert hd(bob_msgs).text == "hey bob"
      assert hd(alice_msgs).text == "hey alice"
    end

    test "an event lands in the peer inbox with its topic" do
      alice_id = Identity.generate()
      bob_id = Identity.generate()

      {:ok, alice} = Agent.start_link(alice_id)
      {:ok, bob} = Agent.start_link(bob_id)
      :ok = Agent.publish(alice)
      :ok = Agent.publish(bob)

      :ok = Agent.emit_event(alice, bob_id.public_key, "dm.new", "01J7Q0")
      Process.sleep(25)
      Agent.poll_mailbox(bob)
      Process.sleep(10)

      assert [msg] = Agent.read_inbox(bob)
      assert msg.kind == :event
      assert msg.meta["topic"] == "dm.new"
      assert msg.text == "01J7Q0"
      assert msg.from == Identity.name(alice_id)
    end

    test "send without session returns error" do
      alice_id = Identity.generate()
      bob_id = Identity.generate()

      {:ok, alice} = Agent.start_link(alice_id)

      :ok = Agent.publish(alice)
      Arc.Control.publish(bob_id)

      assert {:error, :not_found} = Agent.send_message(alice, Identity.name(bob_id), "hello")
    end

    test "reading inbox clears it" do
      alice_id = Identity.generate()
      bob_id = Identity.generate()

      {:ok, alice} = Agent.start_link(alice_id)
      {:ok, bob} = Agent.start_link(bob_id)

      :ok = Agent.publish(alice)
      :ok = Agent.publish(bob)

      {:ok, _} = Agent.connect(alice, Identity.name(bob_id))
      {:ok, _} = Agent.connect(bob, Identity.name(alice_id))

      :ok = Agent.send_message(alice, Identity.name(bob_id), "msg1")
      :ok = Agent.send_message(alice, Identity.name(bob_id), "msg2")

      Process.sleep(10)

      msgs = Agent.read_inbox(bob)
      assert length(msgs) == 2

      assert Agent.read_inbox(bob) == []
    end
  end

  describe "framed replies and replay protection" do
    test "typed framed responses include matching request_id for correlation" do
      client_id = Identity.generate()
      server_id = Identity.generate()
      {runtime_path, manifest_path} = hello_provider_paths()

      try do
        {:ok, client} = Agent.start_link(client_id)
        File.chmod!(runtime_path, 0o755)

        {:ok, server} =
          Agent.start_link(
            server_id,
            serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
          )

        :ok = Agent.publish(client)
        :ok = Agent.publish(server)

        {:ok, _} = Agent.connect(client, Identity.name(server_id))
        {:ok, _} = Agent.connect(server, Identity.name(client_id))

        req_write = Frame.new_request_id()
        req_ls = Frame.new_request_id()

        :ok =
          Agent.send_message(client, Identity.name(server_id), "GET /one",
            request_id: req_write,
            meta: %{"method" => "RAW", "path" => "/"}
          )

        :ok =
          Agent.send_message(client, Identity.name(server_id), "GET /two",
            request_id: req_ls,
            meta: %{"method" => "RAW", "path" => "/"}
          )

        messages = wait_for_messages(client, 100, 2)

        write_reply = Enum.find(messages, &(&1[:request_id] == req_write))
        ls_reply = Enum.find(messages, &(&1[:request_id] == req_ls))

        assert write_reply != nil
        assert write_reply.kind == :response
        assert write_reply.text =~ "GET /one"

        assert ls_reply != nil
        assert ls_reply.kind == :response
        assert ls_reply.text =~ "GET /two"
      after
        :ok
      end
    end

    test "context-aware handlers receive arc_session_id and app_session_id metadata" do
      client_id = Identity.generate()
      server_id = Identity.generate()
      {runtime_path, manifest_path} = context_provider_paths()

      {:ok, client} = Agent.start_link(client_id)
      File.chmod!(runtime_path, 0o755)

      {:ok, server} =
        Agent.start_link(
          server_id,
          serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
        )

      :ok = Agent.publish(client)
      :ok = Agent.publish(server)

      {:ok, _} = Agent.connect(client, Identity.name(server_id))
      {:ok, _} = Agent.connect(server, Identity.name(client_id))

      req_id = Frame.new_request_id()

      :ok =
        Agent.send_message(client, Identity.name(server_id), "hello-context",
          request_id: req_id,
          app_session_id: "sandbox-42",
          meta: %{"method" => "RAW", "path" => "/", "custom" => "yes"}
        )

      [msg] = wait_for_messages(client, 100, 1)
      assert msg.kind == :response
      assert msg.request_id == req_id

      decoded = :json.decode(msg.text)
      assert decoded["message"] == "hello-context"
      assert decoded["app_session_id"] == "sandbox-42"
      assert decoded["method"] == "RAW"
      assert decoded["custom"] == "yes"
      assert is_binary(decoded["arc_session_id"])
      assert byte_size(decoded["arc_session_id"]) == 32
    end

    test "rejects replayed packet with same seq/session_id" do
      alice_id = Identity.generate()
      bob_id = Identity.generate()
      {bob_x_pub, _} = Identity.to_x25519(bob_id)

      {:ok, bob} = Agent.start_link(bob_id)
      :ok = Agent.publish(bob)
      :ok = Arc.Control.publish(alice_id)
      {alice_x_pub, _alice_x_priv} = Identity.to_x25519(alice_id)
      :ok = Arc.Control.publish_keyex(alice_id.public_key, alice_x_pub)

      session = Session.establish(alice_id, bob_id.public_key, bob_x_pub)
      {nonce, ciphertext, seq, _session} = Session.encrypt(session, "replay-test")

      packet =
        Packet.encode(
          alice_id,
          bob_id.public_key,
          session.session_id,
          seq,
          nonce,
          ciphertext,
          ek: session.ek_pub
        )

      send(bob, {:arc_packet, packet})
      send(bob, {:arc_packet, packet})

      Process.sleep(25)
      Agent.poll_mailbox(bob)
      Process.sleep(10)

      messages = Agent.read_inbox(bob)
      assert length(messages) == 1
      assert hd(messages).text == "replay-test"
    end

    test "accepts a v1 packet from a peer on the previous release" do
      alice_id = Identity.generate()
      bob_id = Identity.generate()
      {bob_x_pub, _} = Identity.to_x25519(bob_id)

      {:ok, bob} = Agent.start_link(bob_id)
      :ok = Agent.publish(bob)
      :ok = Arc.Control.publish(alice_id)
      {alice_x_pub, _} = Identity.to_x25519(alice_id)
      :ok = Arc.Control.publish_keyex(alice_id.public_key, alice_x_pub)

      session = Session.establish_v1(alice_id, bob_id.public_key, bob_x_pub)
      {nonce, ciphertext, seq, _} = Session.encrypt(session, "from-v1")

      packet =
        Packet.encode(alice_id, bob_id.public_key, session.session_id, seq, nonce, ciphertext)

      log =
        ExUnit.CaptureLog.capture_log(fn ->
          send(bob, {:arc_packet, packet})
          Process.sleep(25)
          Agent.poll_mailbox(bob)
          Process.sleep(10)
        end)

      assert [%{text: "from-v1"}] = Agent.read_inbox(bob)
      assert log =~ "v1 is deprecated"
    end

    test "the replay guard evicts entries older than twice the skew window" do
      alice_id = Identity.generate()
      {:ok, alice} = Agent.start_link(alice_id)

      now = System.system_time(:millisecond)
      skew = :sys.get_state(alice).allowed_clock_skew_ms
      old_key = {"old-src", "old-sid"}
      fresh_key = {"fresh-src", "fresh-sid"}

      :sys.replace_state(alice, fn state ->
        %{
          state
          | replay_guard: %{
              old_key => %{max_seq: 3, max_ts: now - 2 * skew - 1_000},
              fresh_key => %{max_seq: 1, max_ts: now - skew}
            }
        }
      end)

      send(alice, :sweep_replay_guard)
      guard = :sys.get_state(alice).replay_guard

      refute Map.has_key?(guard, old_key)
      assert Map.has_key?(guard, fresh_key)
    end

    test "rejects stale packet outside clock skew window" do
      alice_id = Identity.generate()
      bob_id = Identity.generate()
      {bob_x_pub, _} = Identity.to_x25519(bob_id)

      {:ok, bob} = Agent.start_link(bob_id)
      :ok = Agent.publish(bob)
      :ok = Arc.Control.publish(alice_id)
      {alice_x_pub, _alice_x_priv} = Identity.to_x25519(alice_id)
      :ok = Arc.Control.publish_keyex(alice_id.public_key, alice_x_pub)

      session = Session.establish(alice_id, bob_id.public_key, bob_x_pub)
      {nonce, ciphertext, seq, _session} = Session.encrypt(session, "stale-test")
      stale_ts = System.system_time(:millisecond) - 300_000

      packet =
        Packet.encode(
          alice_id,
          bob_id.public_key,
          session.session_id,
          seq,
          nonce,
          ciphertext,
          ek: session.ek_pub,
          ts: stale_ts
        )

      send(bob, {:arc_packet, packet})

      Process.sleep(25)
      Agent.poll_mailbox(bob)
      Process.sleep(10)

      assert Agent.read_inbox(bob) == []
    end
  end

  defp context_provider_paths do
    runtime = Path.expand("../../../../../test/fixtures/providers/context-provider.exs", __DIR__)

    manifest =
      Path.expand("../../../../../test/fixtures/providers/context-provider.json", __DIR__)

    {runtime, manifest}
  end

  defp hello_provider_paths do
    runtime = Path.expand("../../../../../test/fixtures/providers/hello-provider.exs", __DIR__)
    manifest = Path.expand("../../../../../test/fixtures/providers/hello-provider.json", __DIR__)
    {runtime, manifest}
  end

  # Agent.read_inbox/1 drains the inbox, so each poll must keep what it read.
  defp wait_for_messages(agent, attempts, min_count, seen \\ []) do
    Agent.poll_mailbox(agent)
    Process.sleep(35)
    messages = seen ++ Agent.read_inbox(agent)

    if length(messages) < min_count and attempts > 1,
      do: wait_for_messages(agent, attempts - 1, min_count, messages),
      else: messages
  end
end
