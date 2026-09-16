defmodule FilesTest do
  use ExUnit.Case
  import ExUnit.CaptureIO

  alias Files.{Command, Envelope, Store, Stdio}

  setup do
    root = Path.join(System.tmp_dir!(), "files-test-#{System.unique_integer([:positive])}")
    :ok = Store.ensure(root)
    on_exit(fn -> File.rm_rf!(root) end)

    {alice_public, alice_private} = :crypto.generate_key(:eddsa, :ed25519)
    {bob_public, _bob_private} = :crypto.generate_key(:eddsa, :ed25519)

    {:ok,
     root: root,
     alice: Base.encode16(alice_public, case: :lower),
     alice_private: alice_private,
     bob: Base.encode16(bob_public, case: :lower)}
  end

  defp token(_label), do: "sealed-v1:" <> Base.encode64(<<1>> <> :crypto.strong_rand_bytes(64))

  defp unsupported_token,
    do: "sealed-v1:" <> Base.encode64(<<2>> <> :crypto.strong_rand_bytes(64))

  defp envelope(owner, private, opts \\ []) do
    name = Keyword.get(opts, :name, token("name"))
    body = Keyword.get(opts, :body, token("body"))
    hash = Envelope.sha256(body)
    message = Envelope.signature_message(owner, name, hash)

    signature =
      :crypto.sign(:eddsa, :none, message, [private, :ed25519]) |> Base.encode16(case: :lower)

    id = Envelope.sha256(message <> "\n" <> signature)

    %{
      "version" => 1,
      "owner" => owner,
      "name" => name,
      "body_hash" => hash,
      "signature" => signature,
      "id" => id,
      "body" => body
    }
  end

  defp request(op, extra),
    do: Map.merge(%{"op" => op}, extra) |> :json.encode() |> IO.iodata_to_binary()

  test "real Ed25519 signed ciphertext round trip stores no plaintext", ctx do
    file = envelope(ctx.alice, ctx.alice_private)

    assert {:ok, %{"id" => id}} =
             Command.run(ctx.root, ctx.alice, request("put", %{"file" => file}))

    assert {:ok, %{"file" => ^file}} =
             Command.run(ctx.root, ctx.alice, request("get", %{"id" => id}))

    assert {:ok, disk} = File.read(Path.join([ctx.root, "objects", ctx.alice, id <> ".json"]))
    assert disk =~ file["body"]
    refute disk =~ "plain private document"
  end

  test "a different authenticated owner cannot get or list files", ctx do
    file = envelope(ctx.alice, ctx.alice_private)
    {:ok, %{"id" => id}} = Command.run(ctx.root, ctx.alice, request("put", %{"file" => file}))
    assert {:error, "not_found"} = Command.run(ctx.root, ctx.bob, request("get", %{"id" => id}))

    assert {:ok, %{"files" => [], "next" => :null}} =
             Command.run(ctx.root, ctx.bob, request("list", %{}))
  end

  test "malicious ids and owner paths are rejected", ctx do
    assert {:error, "invalid_request"} =
             Command.run(ctx.root, ctx.alice, request("get", %{"id" => "../../etc/passwd"}))

    bad = envelope(ctx.alice, ctx.alice_private) |> Map.put("owner", "../../etc/passwd")

    assert {:error, "invalid_envelope"} =
             Command.run(ctx.root, ctx.alice, request("put", %{"file" => bad}))
  end

  test "corruption and replay under another owner fail verification", ctx do
    file = envelope(ctx.alice, ctx.alice_private)

    assert {:error, "invalid_envelope"} =
             Command.run(
               ctx.root,
               ctx.alice,
               request("put", %{"file" => Map.put(file, "body", token("altered"))})
             )

    assert {:error, "invalid_envelope"} =
             Command.run(ctx.root, ctx.bob, request("put", %{"file" => file}))
  end

  test "a signed envelope still rejects an unsupported sealed-box version", ctx do
    invalid = envelope(ctx.alice, ctx.alice_private, body: unsupported_token())

    assert {:error, "invalid_envelope"} =
             Command.run(ctx.root, ctx.alice, request("put", %{"file" => invalid}))
  end

  test "same id retry is idempotent and cannot overwrite", ctx do
    file = envelope(ctx.alice, ctx.alice_private)

    assert {:ok, %{"id" => id}} =
             Command.run(ctx.root, ctx.alice, request("put", %{"file" => file}))

    assert {:ok, %{"id" => ^id}} =
             Command.run(ctx.root, ctx.alice, request("put", %{"file" => file}))

    altered = Map.put(file, "body", token("different"))
    # The altered body also changes the hash, but deliberately leaves the old
    # signature and id, so the accepted object cannot be replaced.
    assert {:error, "invalid_envelope"} =
             Command.run(ctx.root, ctx.alice, request("put", %{"file" => altered}))
  end

  test "list pages sorted signed headers and never includes bodies", ctx do
    files = for _ <- 1..51, do: envelope(ctx.alice, ctx.alice_private)

    Enum.each(files, fn file ->
      assert {:ok, _} = Command.run(ctx.root, ctx.alice, request("put", %{"file" => file}))
    end)

    assert {:ok, %{"files" => first, "next" => cursor}} =
             Command.run(ctx.root, ctx.alice, request("list", %{}))

    assert length(first) == 50
    assert Enum.all?(first, &(not Map.has_key?(&1, "body")))

    assert {:ok, %{"files" => second, "next" => :null}} =
             Command.run(ctx.root, ctx.alice, request("list", %{"after" => cursor}))

    assert length(second) == 1
  end

  test "quota rejects a fresh envelope but permits the accepted retry", ctx do
    first = envelope(ctx.alice, ctx.alice_private)
    assert {:ok, _} = Command.run(ctx.root, ctx.alice, request("put", %{"file" => first}))
    System.put_env("FILES_QUOTA_BYTES", "1")
    on_exit(fn -> System.delete_env("FILES_QUOTA_BYTES") end)
    assert {:ok, _} = Command.run(ctx.root, ctx.alice, request("put", %{"file" => first}))
    second = envelope(ctx.alice, ctx.alice_private)

    assert {:error, "quota_exceeded"} =
             Command.run(ctx.root, ctx.alice, request("put", %{"file" => second}))
  end

  test "stream frames are rejected and never invoke command handling", ctx do
    output =
      capture_io(fn ->
        Stdio.handle_line(
          ctx.root,
          %{"op" => "stream_open", "app_session_id" => "x"}
          |> :json.encode()
          |> IO.iodata_to_binary()
        )
      end)

    assert %{"op" => "stream_error", "app_session_id" => "x"} = :json.decode(String.trim(output))
  end

  test "stdio replies keep objects as objects and malformed input does not crash", ctx do
    line =
      %{
        "op" => "request",
        "request_id" => "request-1",
        "from" => ctx.alice,
        "message" => request("list", %{"after" => :null})
      }
      |> :json.encode()
      |> IO.iodata_to_binary()

    malformed = capture_io(fn -> Stdio.handle_line(ctx.root, "{") end)
    assert %{"op" => "error", "error" => "invalid_request"} = :json.decode(String.trim(malformed))

    output = capture_io(fn -> Stdio.handle_line(ctx.root, line) end)

    assert %{
             "op" => "reply",
             "request_id" => "request-1",
             "reply" => %{"files" => [], "next" => :null}
           } =
             :json.decode(String.trim(output))
  end

  test "corrupt stored objects fail explicitly and list never hides them", ctx do
    file = envelope(ctx.alice, ctx.alice_private)
    {:ok, %{"id" => id}} = Command.run(ctx.root, ctx.alice, request("put", %{"file" => file}))
    target = Path.join([ctx.root, "objects", ctx.alice, id <> ".json"])
    File.write!(target, "not-json")

    assert {:error, "corrupt_storage"} =
             Command.run(ctx.root, ctx.alice, request("get", %{"id" => id}))

    assert {:error, "corrupt_storage"} = Command.run(ctx.root, ctx.alice, request("list", %{}))
  end

  test "strict keys and ids reject a trailing newline", ctx do
    assert {:error, "invalid_request"} =
             Command.run(ctx.root, ctx.alice <> "\n", request("list", %{}))

    assert {:error, "invalid_request"} =
             Command.run(
               ctx.root,
               ctx.alice,
               request("get", %{"id" => String.duplicate("0", 64) <> "\n"})
             )
  end

  test "a symlink object makes reads and writes fail safely", ctx do
    file = envelope(ctx.alice, ctx.alice_private)
    target = Path.join([ctx.root, "objects", ctx.alice, file["id"] <> ".json"])
    File.mkdir_p!(Path.dirname(target))
    :ok = File.ln_s("/dev/null", target)

    assert {:error, "corrupt_storage"} =
             Command.run(ctx.root, ctx.alice, request("get", %{"id" => file["id"]}))

    assert {:error, "corrupt_storage"} =
             Command.run(ctx.root, ctx.alice, request("put", %{"file" => file}))
  end

  test "a symlink owner directory makes reads and lists fail safely", ctx do
    owner_dir = Path.join([ctx.root, "objects", ctx.alice])
    :ok = File.ln_s("/dev/null", owner_dir)
    id = String.duplicate("0", 64)

    assert {:error, "corrupt_storage"} =
             Command.run(ctx.root, ctx.alice, request("get", %{"id" => id}))

    assert {:error, "corrupt_storage"} = Command.run(ctx.root, ctx.alice, request("list", %{}))
  end

  test "quota configuration must be a positive integer", _ctx do
    System.put_env("FILES_QUOTA_BYTES", "0")
    on_exit(fn -> System.delete_env("FILES_QUOTA_BYTES") end)
    assert {:error, "invalid FILES_QUOTA_BYTES"} = Files.Config.validate()
  end
end
