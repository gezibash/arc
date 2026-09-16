defmodule AgoraTest do
  use ExUnit.Case
  import ExUnit.CaptureIO

  alias Agora.{Command, Post, Store, Stdio}

  setup do
    root = Path.join(System.tmp_dir!(), "agora-test-#{System.unique_integer([:positive])}")
    :ok = Store.ensure(root)
    on_exit(fn -> File.rm_rf!(root) end)

    {board_public, _board_private} = :crypto.generate_key(:eddsa, :ed25519)
    {alice_public, alice_private} = :crypto.generate_key(:eddsa, :ed25519)
    {bob_public, bob_private} = :crypto.generate_key(:eddsa, :ed25519)

    {:ok,
     root: root,
     board: Base.encode16(board_public, case: :lower),
     alice: Base.encode16(alice_public, case: :lower),
     alice_private: alice_private,
     bob: Base.encode16(bob_public, case: :lower),
     bob_private: bob_private}
  end

  defp post(author, private, board, opts \\ []) do
    body = Keyword.get(opts, :body, "A public note")
    parent = Keyword.get(opts, :parent, :null)
    created_at = Keyword.get(opts, :created_at, System.system_time(:second))

    nonce =
      Keyword.get(opts, :nonce, :crypto.strong_rand_bytes(16) |> Base.encode16(case: :lower))

    message = Post.signature_message(author, board, body, parent, created_at, nonce)
    raw = :crypto.sign(:eddsa, :none, message, [private, :ed25519])

    %{
      "version" => 1,
      "author" => author,
      "board" => board,
      "body" => body,
      "parent" => parent,
      "created_at" => created_at,
      "nonce" => nonce,
      "signature" => Base.encode16(raw, case: :lower),
      "id" => Post.sha256(message <> raw)
    }
  end

  defp request(op, extra),
    do: Map.merge(%{"op" => op}, extra) |> :json.encode() |> IO.iodata_to_binary()

  test "accepts an authenticated signed post and exact retries", ctx do
    value = post(ctx.alice, ctx.alice_private, ctx.board)

    assert {:ok, %{"post" => ^value}} =
             Command.run(ctx.root, ctx.board, ctx.alice, request("post", %{"post" => value}))

    assert {:ok, %{"post" => ^value}} =
             Command.run(ctx.root, ctx.board, ctx.alice, request("post", %{"post" => value}))

    assert {:ok, %{"post" => ^value}} =
             Command.run(ctx.root, ctx.board, ctx.bob, request("read", %{"id" => value["id"]}))
  end

  test "binds author, board, canonical fields, signature and freshness", ctx do
    value = post(ctx.alice, ctx.alice_private, ctx.board)

    assert {:error, "invalid_post"} =
             Command.run(ctx.root, ctx.board, ctx.bob, request("post", %{"post" => value}))

    assert {:error, "invalid_post"} =
             Command.run(
               ctx.root,
               ctx.board,
               ctx.alice,
               request("post", %{"post" => Map.put(value, "body", "altered")})
             )

    assert {:error, "invalid_post"} =
             Command.run(
               ctx.root,
               ctx.board,
               ctx.alice,
               request("post", %{"post" => Map.put(value, "unexpected", true)})
             )

    stale =
      post(ctx.alice, ctx.alice_private, ctx.board, created_at: System.system_time(:second) - 301)

    assert {:error, "stale_post"} =
             Command.run(ctx.root, ctx.board, ctx.alice, request("post", %{"post" => stale}))
  end

  test "requires an existing parent and pages direct replies in acceptance order", ctx do
    root = post(ctx.alice, ctx.alice_private, ctx.board, body: "Root")
    reply = post(ctx.bob, ctx.bob_private, ctx.board, parent: root["id"], body: "Reply")
    nested = post(ctx.alice, ctx.alice_private, ctx.board, parent: reply["id"], body: "Nested")
    missing = post(ctx.alice, ctx.alice_private, ctx.board, parent: String.duplicate("a", 64))

    assert {:error, "not_found"} =
             Command.run(ctx.root, ctx.board, ctx.alice, request("post", %{"post" => missing}))

    for {from, value} <- [{ctx.alice, root}, {ctx.bob, reply}, {ctx.alice, nested}] do
      assert {:ok, _} =
               Command.run(ctx.root, ctx.board, from, request("post", %{"post" => value}))
    end

    assert {:ok, %{"posts" => [^root], "next" => :null}} =
             Command.run(ctx.root, ctx.board, ctx.bob, request("feed", %{"limit" => 20}))

    assert {:ok, %{"post" => ^root, "posts" => [^reply], "next" => :null}} =
             Command.run(ctx.root, ctx.board, ctx.bob, request("thread", %{"id" => root["id"]}))

    assert {:ok, %{"post" => ^reply, "posts" => [^nested], "next" => :null}} =
             Command.run(ctx.root, ctx.board, ctx.bob, request("thread", %{"id" => reply["id"]}))
  end

  test "feed pagination uses acceptance sequence and validates bounds", ctx do
    posts =
      for number <- 1..3 do
        value = post(ctx.alice, ctx.alice_private, ctx.board, body: "Post #{number}")

        assert {:ok, _} =
                 Command.run(ctx.root, ctx.board, ctx.alice, request("post", %{"post" => value}))

        value
      end

    assert {:ok, %{"posts" => [first], "next" => 1}} =
             Command.run(ctx.root, ctx.board, ctx.bob, request("feed", %{"limit" => 1}))

    assert first == hd(posts)

    assert {:ok, %{"posts" => [second, third], "next" => :null}} =
             Command.run(
               ctx.root,
               ctx.board,
               ctx.bob,
               request("feed", %{"after" => 1, "limit" => 2})
             )

    assert [second, third] == tl(posts)

    assert {:error, "invalid_request"} =
             Command.run(ctx.root, ctx.board, ctx.bob, request("feed", %{"limit" => 51}))
  end

  test "a post written before a state commit is recovered on restart", ctx do
    value = post(ctx.alice, ctx.alice_private, ctx.board)
    target = Path.join([ctx.root, "posts", value["id"] <> ".json"])
    File.write!(target, value |> :json.encode() |> IO.iodata_to_binary())

    assert {:ok, %{"posts" => [^value], "next" => :null}} =
             Command.run(ctx.root, ctx.board, ctx.bob, request("feed", %{}))

    assert {:ok, %{"post" => ^value}} =
             Command.run(ctx.root, ctx.board, ctx.alice, request("post", %{"post" => value}))
  end

  test "corrupt stored posts fail explicitly and are never hidden", ctx do
    value = post(ctx.alice, ctx.alice_private, ctx.board)

    assert {:ok, _} =
             Command.run(ctx.root, ctx.board, ctx.alice, request("post", %{"post" => value}))

    File.write!(Path.join([ctx.root, "posts", value["id"] <> ".json"]), "bad")

    assert {:error, "corrupt_storage"} =
             Command.run(ctx.root, ctx.board, ctx.bob, request("feed", %{}))
  end

  test "state is bound to its board and indexed parents must match signed posts", ctx do
    first = post(ctx.alice, ctx.alice_private, ctx.board, body: "First")
    second = post(ctx.alice, ctx.alice_private, ctx.board, body: "Second")

    for value <- [first, second] do
      assert {:ok, _} =
               Command.run(ctx.root, ctx.board, ctx.alice, request("post", %{"post" => value}))
    end

    state_path = Path.join(ctx.root, "state.json")
    state = state_path |> File.read!() |> :json.decode()
    [one, two] = state["records"]
    bad_parent = %{state | "records" => [one, %{two | "parent" => first["id"]}]}
    File.write!(state_path, bad_parent |> :json.encode() |> IO.iodata_to_binary())

    assert {:error, "corrupt_storage"} =
             Command.run(ctx.root, ctx.board, ctx.bob, request("feed", %{}))

    File.write!(
      state_path,
      %{state | "board" => String.duplicate("a", 64)} |> :json.encode() |> IO.iodata_to_binary()
    )

    assert {:error, "corrupt_storage"} =
             Command.run(ctx.root, ctx.board, ctx.bob, request("feed", %{}))
  end

  test "recognized abandoned post temporary files do not block recovery", ctx do
    value = post(ctx.alice, ctx.alice_private, ctx.board)

    assert {:ok, _} =
             Command.run(ctx.root, ctx.board, ctx.alice, request("post", %{"post" => value}))

    temp = Path.join([ctx.root, "posts", value["id"] <> ".json.tmp-abandoned"])
    File.write!(temp, "partial")

    assert {:ok, %{"posts" => [^value]}} =
             Command.run(ctx.root, ctx.board, ctx.bob, request("feed", %{}))
  end

  test "quota applies to new posts and allows accepted retries", ctx do
    System.put_env("AGORA_MAX_POSTS", "1")
    on_exit(fn -> System.delete_env("AGORA_MAX_POSTS") end)
    first = post(ctx.alice, ctx.alice_private, ctx.board)
    second = post(ctx.alice, ctx.alice_private, ctx.board, body: "Second")

    assert {:ok, _} =
             Command.run(ctx.root, ctx.board, ctx.alice, request("post", %{"post" => first}))

    assert {:ok, _} =
             Command.run(ctx.root, ctx.board, ctx.alice, request("post", %{"post" => first}))

    assert {:error, "quota_exceeded"} =
             Command.run(ctx.root, ctx.board, ctx.alice, request("post", %{"post" => second}))
  end

  test "runtime lock refuses concurrent writers and recovers a dead pid lock", ctx do
    assert {:ok, lock} = Store.acquire_lock(ctx.root)
    assert {:error, "storage_locked"} = Store.acquire_lock(ctx.root)
    assert :ok = Store.release_lock(lock)

    lock_dir = Path.join(ctx.root, ".lock")
    assert :ok = File.mkdir(lock_dir)
    File.write!(Path.join(lock_dir, "pid"), "999999")

    case System.cmd("/bin/ps", ["-p", "999999", "-o", "pid="], stderr_to_stdout: true) do
      {output, 1} ->
        if String.trim(output) == "" do
          assert {:ok, recovered} = Store.acquire_lock(ctx.root)
          assert :ok = Store.release_lock(recovered)
        else
          assert {:error, "storage_locked"} = Store.acquire_lock(ctx.root)
        end

      _ ->
        assert {:error, "storage_locked"} = Store.acquire_lock(ctx.root)
    end
  end

  test "stdio keeps replies as objects and rejects streams", ctx do
    line =
      %{
        "op" => "request",
        "request_id" => "request-1",
        "from" => ctx.alice,
        "message" => request("feed", %{})
      }
      |> :json.encode()
      |> IO.iodata_to_binary()

    output = capture_io(fn -> Stdio.handle_line(ctx.root, ctx.board, line) end)

    assert %{"op" => "reply", "reply" => %{"posts" => [], "next" => :null}} =
             :json.decode(String.trim(output))

    output =
      capture_io(fn ->
        Stdio.handle_line(
          ctx.root,
          ctx.board,
          %{"op" => "stream_open", "app_session_id" => "x"}
          |> :json.encode()
          |> IO.iodata_to_binary()
        )
      end)

    assert %{"op" => "stream_error", "app_session_id" => "x"} = :json.decode(String.trim(output))
  end
end
