defmodule DmTest do
  use ExUnit.Case

  alias Dm.{Command, ULID}

  @alice String.duplicate("a1", 32)
  @bob String.duplicate("b2", 32)
  @carol String.duplicate("c3", 32)

  setup do
    root = Path.join(System.tmp_dir!(), "dm-test-#{System.unique_integer([:positive])}")
    :ok = Dm.Store.ensure(root)
    on_exit(fn -> File.rm_rf!(root) end)
    {:ok, root: root}
  end

  defp token(text), do: "sealed-v1:" <> Base.encode64("fake|" <> text)
  defp body(text), do: token(text <> "@peer") <> "\n" <> token(text <> "@self") <> "\n"

  defp send(root, from, to, text, extra \\ "") do
    {:ok, "id: " <> id} = Command.run(root, from, "send #{to} #{extra}\n" <> body(text))
    id
  end

  defp lines({:ok, text}), do: String.split(text, "\n")

  test "ulid is 26 chars and monotonic" do
    a = ULID.generate()
    b = ULID.generate()
    assert ULID.valid?(a)
    assert String.length(a) == 26
    assert a < b
    refute ULID.valid?("nope")
  end

  test "caller key is required", %{root: root} do
    assert {:error, "forbidden" <> _} = Command.run(root, "", "whoami")
    assert {:error, "forbidden" <> _} = Command.run(root, "abc", "whoami")
    assert {:ok, @alice} = Command.run(root, @alice, "whoami")
  end

  test "send stores one sealed copy per mailbox and a delivered receipt", %{root: root} do
    id = send(root, @alice, @bob, "hi")

    {:ok, bob_msg} = Dm.Store.get_message(root, @bob, id)
    {:ok, alice_msg} = Dm.Store.get_message(root, @alice, id)

    assert bob_msg["body"] == token("hi@peer")
    assert alice_msg["body"] == token("hi@self")
    assert bob_msg["from"] == @alice and bob_msg["to"] == @bob
    assert bob_msg["enc"] == "sealed-v1"
    assert [%{"event" => "delivered"}] = Dm.Store.receipts(root, @bob, id)
    assert [] = Dm.Store.receipts(root, @alice, id)
  end

  test "no plaintext reaches the disk", %{root: root} do
    send(root, @alice, @bob, "the secret words")

    # grep exits 1 when nothing matches, which is the result we want.
    assert {"", 1} = System.cmd("grep", ["-rl", "secret words", root], stderr_to_stdout: true)
  end

  test "send rejects an unsealed or malformed body", %{root: root} do
    assert {:error, "unsealed" <> _} = Command.run(root, @alice, "send #{@bob}\nplain text\nmore")
    assert {:error, "unsealed" <> _} = Command.run(root, @alice, "send #{@bob}\n" <> token("x"))
    assert {:error, "unsealed" <> _} = Command.run(root, @alice, "send #{@bob}")
  end

  test "send rejects a body over the cap", %{root: root} do
    big = "sealed-v1:" <> String.duplicate("A", Dm.Config.max_body_bytes())
    assert {:error, "too_large" <> _} = Command.run(root, @alice, "send #{@bob}\n#{big}\n#{big}")
  end

  test "send rejects a bad peer address", %{root: root} do
    assert {:error, "invalid_address"} = Command.run(root, @alice, "send bob\n" <> body("x"))
  end

  test "send carries reply_to", %{root: root} do
    first = send(root, @alice, @bob, "q")
    second = send(root, @bob, @alice, "a", "--reply-to \"#{first}\"")
    {:ok, out} = Command.run(root, @alice, "read #{second}")
    assert out =~ "reply_to: #{first}"
  end

  test "inbox lists both directions, newest last, and honours --unread", %{root: root} do
    m1 = send(root, @alice, @bob, "one")
    m2 = send(root, @bob, @alice, "two")

    assert [l1, l2] = lines(Command.run(root, @bob, "inbox"))
    assert l1 =~ "#{m1}\tin\t#{@alice}\t"
    assert l2 =~ "#{m2}\tout\t#{@alice}\t"
    assert String.ends_with?(l1, "\t#{byte_size(token("one@peer"))}")

    assert [^l1] = lines(Command.run(root, @bob, "inbox --unread \"true\""))
    {:ok, _} = Command.run(root, @bob, "read #{m1}")
    assert {:ok, "no messages"} = Command.run(root, @bob, "inbox --unread \"true\"")
  end

  test "inbox --since and --limit", %{root: root} do
    ids = for i <- 1..3, do: send(root, @alice, @bob, "m#{i}")

    assert [l] = lines(Command.run(root, @bob, "inbox --since #{Enum.at(ids, 1)}"))
    assert String.starts_with?(l, Enum.at(ids, 2))

    assert [l] = lines(Command.run(root, @bob, "inbox --limit 1"))
    assert String.starts_with?(l, Enum.at(ids, 2))
  end

  test "read returns the sealed body and writes one read receipt", %{root: root} do
    id = send(root, @alice, @bob, "hello")

    {:ok, out} = Command.run(root, @bob, "read #{id}")
    assert out =~ "id: #{id}\nfrom: #{@alice}\nto: #{@bob}\n"
    assert out =~ "body: " <> token("hello@peer")

    {:ok, _} = Command.run(root, @bob, "read #{id}")
    events = root |> Dm.Store.receipts(@bob, id) |> Enum.map(& &1["event"])
    assert events == ["delivered", "read"]
  end

  test "read of another mailbox's message is not found", %{root: root} do
    id = send(root, @alice, @bob, "hello")
    assert {:error, "not_found"} = Command.run(root, @carol, "read #{id}")
    assert {:error, "not_found"} = Command.run(root, @bob, "read nonsense")
  end

  test "status shows delivered and read to the sender only", %{root: root} do
    id = send(root, @alice, @bob, "hello")

    assert {:ok, "delivered " <> _} = Command.run(root, @alice, "status #{id}")
    {:ok, _} = Command.run(root, @bob, "ack #{id}")
    assert ["delivered " <> _, "read " <> _] = lines(Command.run(root, @alice, "status #{id}"))
    assert {:error, "forbidden" <> _} = Command.run(root, @bob, "status #{id}")
  end

  test "ack refuses an outbound message", %{root: root} do
    id = send(root, @alice, @bob, "hello")
    assert {:error, "forbidden" <> _} = Command.run(root, @alice, "ack #{id}")
  end

  test "archive hides from inbox but not from thread", %{root: root} do
    id = send(root, @alice, @bob, "hello")
    assert {:ok, "archived 1"} = Command.run(root, @bob, "archive #{id}")
    assert {:ok, "no messages"} = Command.run(root, @bob, "inbox")
    assert [l] = lines(Command.run(root, @bob, "thread #{@alice}"))
    assert String.starts_with?(l, id)
  end

  test "thread filters by peer in both directions", %{root: root} do
    a = send(root, @alice, @bob, "1")
    b = send(root, @bob, @alice, "2")
    _c = send(root, @carol, @alice, "3")

    assert [l1, l2] = lines(Command.run(root, @alice, "thread #{@bob}"))
    assert String.starts_with?(l1, a)
    assert String.starts_with?(l2, b)
  end

  test "block refuses sends and unblock restores them", %{root: root} do
    assert {:ok, _} = Command.run(root, @bob, "block #{@alice}")
    assert {:ok, @alice} = Command.run(root, @bob, "blocked")
    assert {:error, "blocked"} = Command.run(root, @alice, "send #{@bob}\n" <> body("x"))
    assert {:ok, _} = Command.run(root, @bob, "unblock #{@alice}")
    assert {:ok, "no blocked keys"} = Command.run(root, @bob, "blocked")
    assert "id: " <> _ = elem(Command.run(root, @alice, "send #{@bob}\n" <> body("x")), 1)
  end

  test "send to self stores one copy", %{root: root} do
    id = send(root, @alice, @alice, "note")
    assert [l] = lines(Command.run(root, @alice, "inbox"))
    assert String.starts_with?(l, id)
  end

  test "conversations lists peers newest first with unread counts and last body", %{root: root} do
    a1 = send(root, @alice, @bob, "hi bob")
    _c1 = send(root, @carol, @bob, "hi from carol")
    _a2 = send(root, @alice, @bob, "again")

    {:ok, out} = Command.run(root, @bob, "conversations")
    [header, l1, l2] = String.split(out, "\n")

    assert header == "3 unread in 2 conversations"
    assert [@alice, _id, _t, "2", "-", body] = String.split(l1, "\t")
    assert body == token("again@peer")
    assert [@carol, _, _, "1", "-", _] = String.split(l2, "\t")

    {:ok, _} = Command.run(root, @bob, "read #{a1}")
    {:ok, out} = Command.run(root, @bob, "conversations --limit 1")
    assert ["1 unread in 1 conversations", l1] = String.split(out, "\n")
    assert [@alice, _, _, "1", "-", _] = String.split(l1, "\t")
  end

  test "conversations includes peers you only sent to", %{root: root} do
    send(root, @alice, @bob, "hello")
    {:ok, out} = Command.run(root, @alice, "conversations")
    assert ["0 unread in 1 conversations", l] = String.split(out, "\n")
    assert [@bob, _, _, "0", "-", body] = String.split(l, "\t")
    assert body == token("hello@self")
  end

  test "mute stops unread counting but keeps delivery", %{root: root} do
    assert {:ok, "muted " <> _} = Command.run(root, @bob, "mute #{@alice}")
    assert {:ok, @alice} = Command.run(root, @bob, "muted")

    id = send(root, @alice, @bob, "psst")
    assert {:ok, "no messages"} = Command.run(root, @bob, "inbox --unread \"true\"")
    assert [l] = lines(Command.run(root, @bob, "inbox"))
    assert String.starts_with?(l, id)

    {:ok, out} = Command.run(root, @bob, "conversations")
    assert ["0 unread in 1 conversations", l] = String.split(out, "\n")
    assert [@alice, _, _, "0", "muted", _] = String.split(l, "\t")

    assert {:ok, "unmuted " <> _} = Command.run(root, @bob, "unmute #{@alice}")
    assert {:ok, "no muted keys"} = Command.run(root, @bob, "muted")
    assert [_] = lines(Command.run(root, @bob, "inbox --unread \"true\""))
  end

  test "receipts off hides read receipts from the sender", %{root: root} do
    assert {:ok, "receipts on"} = Command.run(root, @bob, "settings")
    assert {:ok, "receipts off"} = Command.run(root, @bob, "settings receipts off")
    assert {:error, "invalid_setting" <> _} = Command.run(root, @bob, "settings colour blue")

    id = send(root, @alice, @bob, "hello")
    {:ok, _} = Command.run(root, @bob, "read #{id}")

    assert ["delivered " <> _] = lines(Command.run(root, @alice, "status #{id}"))
    assert {:ok, "receipts on"} = Command.run(root, @bob, "settings receipts on")
    assert ["delivered " <> _, "read " <> _] = lines(Command.run(root, @alice, "status #{id}"))
  end

  test "unknown command and help", %{root: root} do
    assert {:error, "unknown_command nope"} = Command.run(root, @alice, "nope")
    assert {:ok, "dm commands" <> _} = Command.run(root, @alice, "help")
  end

  test "stdio loop replies to a request line", %{root: root} do
    line =
      :json.encode(%{
        "op" => "request",
        "message" => "whoami",
        "from" => @alice,
        "request_id" => "r1"
      })

    out =
      ExUnit.CaptureIO.capture_io(fn ->
        Dm.Stdio.handle_line(root, IO.iodata_to_binary(line))
      end)

    assert :json.decode(String.trim(out)) == %{
             "op" => "reply",
             "request_id" => "r1",
             "reply" => @alice
           }
  end
end
