defmodule Arc.Data.AgoraTest do
  use ExUnit.Case, async: true

  alias Arc.Data.{Agora, Toolbox}
  alias Arc.Identity

  setup do
    %{alice: Identity.generate(), board: Identity.generate() |> Identity.encode_public_key()}
  end

  test "signatures survive JSON round trips and bind every field", %{alice: alice, board: board} do
    body = "Human & agent\n\"quotes\" \\ slash / café 🏛\u0000"
    assert {:ok, post} = Agora.sign(alice, board, body)
    assert :ok = Agora.verify(:json.decode(encode(post)), board)
    assert post["body"] == body

    for {key, value} <- [
          {"body", "changed"},
          {"author", board},
          {"board", String.duplicate("a", 64)},
          {"parent", String.duplicate("b", 64)},
          {"created_at", 0},
          {"nonce", String.duplicate("0", 32)},
          {"signature", String.duplicate("0", 128)},
          {"id", String.duplicate("0", 64)},
          {"version", 2},
          {"extra", true}
        ] do
      assert {:error, _} = Agora.verify(Map.put(post, key, value), board)
    end
  end

  test "fixed independent signature vector and cross-board replay protection", %{board: board} do
    identity = Identity.from_seed(:binary.copy(<<7>>, 32))
    author = Identity.encode_public_key(identity)

    post = %{
      "version" => 1,
      "author" => author,
      "board" => board,
      "body" => "hello",
      "parent" => :null,
      "created_at" => 123,
      "nonce" => String.duplicate("a", 32)
    }

    message =
      "arc-agora-post-v1\n[1,\"#{author}\",\"#{board}\",\"hello\",null,123,\"#{post["nonce"]}\"]"

    signature = :crypto.sign(:eddsa, :none, message, [identity.secret_key, :ed25519])

    post =
      Map.merge(post, %{
        "signature" => hex(signature),
        "id" => hex(:crypto.hash(:sha256, message <> signature))
      })

    assert :ok = Agora.verify(post, board)
    assert {:error, _} = Agora.verify(post, author)
  end

  test "bounds signed bodies and rejects malformed arguments", %{alice: alice, board: board} do
    assert {:ok, _} = Agora.sign(alice, board, String.duplicate("x", 4096))

    for body <- [nil, "", " \n", <<255>>, String.duplicate("x", 4097)] do
      assert {:error, _} = Agora.sign(alice, board, body)
    end

    assert {:error, _} = Agora.sign(alice, board, "hello", "../outside")
    assert {:error, _} = Agora.sign(alice, String.upcase(board), "hello")

    assert {:error, _} =
             Agora.prepare("reply", %{"body" => "hello"}, %{board: board, identity: alice})
  end

  test "untrusted responses cannot substitute another signed post or add unchecked output", %{
    alice: alice,
    board: board
  } do
    assert {:ok, _, expected} =
             Agora.prepare("post", %{"body" => "hello"}, %{board: board, identity: alice})

    post = expected.request["post"]
    assert {:ok, _} = Agora.finish(encode(%{"post" => post}), expected)
    assert {:ok, other} = Agora.sign(alice, board, "another valid post")

    for response <- [
          %{"post" => other},
          %{"post" => post, "message" => "unchecked"},
          %{"post" => Map.put(post, "body", "forged")}
        ] do
      assert {:error, _} = Agora.finish(encode(response), expected)
    end

    assert {:error, _} = Agora.finish(encode(%{"post" => post}) <> "trailing text", expected)
    assert {:error, _} = Agora.finish(String.duplicate("x", 1_500_001), expected)
    assert {:error, _} = Agora.finish("null", expected)
  end

  test "feed and thread validate parents, pagination, duplicate entries, and ids", %{
    alice: alice,
    board: board
  } do
    {:ok, root} = Agora.sign(alice, board, "root")
    {:ok, child} = Agora.sign(alice, board, "reply", root["id"])
    {:ok, other} = Agora.sign(alice, board, "other")
    {:ok, _, feed} = Agora.prepare("feed", %{"limit" => "1"}, %{board: board})
    assert {:ok, _} = Agora.finish(encode(%{"posts" => [root], "next" => 1}), feed)
    assert {:error, _} = Agora.finish(encode(%{"posts" => [child], "next" => :null}), feed)
    assert {:error, _} = Agora.finish(encode(%{"posts" => [root, other], "next" => :null}), feed)
    {:ok, _, feed} = Agora.prepare("feed", %{"after" => "4"}, %{board: board})
    assert {:error, _} = Agora.finish(encode(%{"posts" => [root], "next" => 4}), feed)
    assert {:error, _} = Agora.finish(encode(%{"posts" => [root, root], "next" => :null}), feed)
    assert {:error, _} = Agora.finish(encode(%{"posts" => ["malformed"], "next" => :null}), feed)
    {:ok, _, thread} = Agora.prepare("thread", %{"id" => root["id"]}, %{board: board})

    assert {:ok, _} =
             Agora.finish(encode(%{"post" => root, "posts" => [child], "next" => :null}), thread)

    assert {:error, _} =
             Agora.finish(encode(%{"post" => other, "posts" => [child], "next" => :null}), thread)

    assert {:error, _} =
             Agora.finish(encode(%{"post" => root, "posts" => [other], "next" => :null}), thread)

    {:ok, _, read} = Agora.prepare("read", %{"id" => root["id"]}, %{board: board})
    assert {:ok, _} = Agora.finish(encode(%{"post" => root}), read)
    assert {:error, _} = Agora.finish(encode(%{"post" => other}), read)

    for values <- [
          %{"limit" => "0"},
          %{"limit" => "51"},
          %{"after" => "-1"},
          %{"after" => "1junk"}
        ] do
      assert {:error, _} = Agora.prepare("feed", values, %{board: board})
    end
  end

  test "installed toolbox derives board locally and retains mandatory verification", %{
    alice: alice,
    board: board
  } do
    tool = tool(board)

    assert {:ok, built} =
             Toolbox.build_invocation(tool, ["post", "Hello", "citizens"], %{
               identity: alice,
               board: "untrusted"
             })

    post = :json.decode(built.input)["post"]
    assert post["body"] == "Hello citizens"
    assert post["author"] == Identity.encode_public_key(alice)
    assert post["board"] == board
    assert :ok = Agora.verify(post, board)
    assert {:ok, _} = Toolbox.finish_reply(%{text: encode(%{"post" => post})}, built)

    assert {:error, _} =
             Toolbox.finish_reply(
               %{text: encode(%{"post" => Map.put(post, "body", "forged")})},
               Map.put(built, :output_opts, %{raw: true})
             )

    assert {:error, _} = Toolbox.build_invocation(tool, ["post", "hello"])

    assert {:error, _} =
             Toolbox.build_invocation(
               put_in(tool, ["provider", "public_key"], nil),
               ["post", "hello"],
               %{identity: alice}
             )

    assert {:error, _} =
             Toolbox.render_input(hd(Toolbox.cli_commands(tool["capability"])), [], %{}, %{
               identity: alice
             })
  end

  test "toolbox supports agent signer and fails closed for incompatible versions and streams", %{
    alice: alice,
    board: board
  } do
    context = %{sign_post: fn target, body, parent -> Agora.sign(alice, target, body, parent) end}
    assert {:ok, built} = Toolbox.build_invocation(tool(board), ["post", "hello"], context)
    assert :ok = Agora.verify(:json.decode(built.input)["post"], board)

    for version <- [1, 3, 5] do
      tool = put_in(tool(board), ["capability", "interfaces", "cli", "version"], version)
      assert {:error, _} = Toolbox.build_invocation(tool, ["post", "hello"], context)
    end

    tool = put_in(tool(board), ["capability", "invocation", "mode"], "stream")
    assert {:error, _} = Toolbox.build_invocation(tool, ["post", "hello"], context)
  end

  defp tool(board) do
    %{
      "provider" => %{"public_key" => board},
      "capability" => %{
        "invocation" => %{"method" => "RAW", "path" => "/"},
        "interfaces" => %{
          "cli" => %{
            "version" => 4,
            "namespace" => "agora",
            "commands" => [
              %{
                "path" => ["post"],
                "args" => [
                  %{
                    "name" => "body",
                    "kind" => "positional",
                    "required" => true,
                    "variadic" => true
                  }
                ],
                "input" => %{"source" => "agora", "operation" => "post"}
              }
            ]
          }
        }
      }
    }
  end

  defp encode(value), do: value |> :json.encode() |> IO.iodata_to_binary()
  defp hex(value), do: Base.encode16(value, case: :lower)
end
