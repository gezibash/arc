defmodule Arc.Data.ToolboxTest do
  use ExUnit.Case, async: true

  alias Arc.Data.Toolbox

  defp tool(input, args) do
    %{
      "command" => "journal",
      "capability" => %{
        "invocation" => %{"method" => "RAW", "path" => "/"},
        "interfaces" => %{
          "cli" => %{
            "version" => 1,
            "namespace" => "journal",
            "commands" => [%{"path" => ["write"], "args" => args, "input" => input}]
          }
        }
      }
    }
  end

  @args [
    %{"name" => "addr", "kind" => "positional", "type" => "string", "required" => true},
    %{"name" => "body", "kind" => "option", "flag" => "--body", "type" => "string"},
    %{"name" => "draft", "kind" => "option", "flag" => "--draft", "type" => "boolean"}
  ]

  test "plain placeholders substitute raw values" do
    tool =
      tool(%{"source" => "template", "template" => "write {{addr}} --body \"{{body}}\""}, @args)

    assert {:ok, %{input: ~s(write inbox --body "He said "hi"")}} =
             Toolbox.build_invocation(tool, ["write", "inbox", "--body", ~s(He said "hi")])
  end

  test "json filter renders the value as a JSON literal" do
    tool =
      tool(%{"source" => "template", "template" => "write {{addr}} --body {{body|json}}"}, @args)

    assert {:ok, %{input: ~s(write inbox --body "He said \\"hi\\"\\n--flag")}} =
             Toolbox.build_invocation(tool, ["write", "inbox", "--body", "He said \"hi\"\n--flag"])
  end

  test "json filter renders a missing value as null" do
    tool = tool(%{"source" => "template", "template" => "write {{addr}} {{body|json}}"}, @args)

    assert {:ok, %{input: "write inbox null"}} =
             Toolbox.build_invocation(tool, ["write", "inbox"])
  end

  test "shell filter single-quotes the value" do
    tool =
      tool(%{"source" => "template", "template" => "write {{addr}} --body {{body|shell}}"}, @args)

    assert {:ok, %{input: ~s(write inbox --body 'it'\\''s "fine" --flag')}} =
             Toolbox.build_invocation(tool, ["write", "inbox", "--body", ~s(it's "fine" --flag)])
  end

  test "unknown filter is rejected" do
    tool = tool(%{"source" => "template", "template" => "write {{addr|base64}}"}, @args)

    assert {:error, {:invalid_template, "unknown filter base64"}} =
             Toolbox.build_invocation(tool, ["write", "inbox"])
  end

  describe "pubkey and seal filters" do
    setup do
      bob = Arc.Identity.generate()
      {bob_x, _} = Arc.Identity.to_x25519(bob)
      nokey = Arc.Identity.generate()
      me = Arc.Identity.generate()

      entries = %{
        Arc.Identity.name(bob) => [
          %{public_key: bob.public_key, x25519_public: bob_x, name: "bob"}
        ],
        Arc.Identity.name(nokey) => [
          %{public_key: nokey.public_key, x25519_public: nil, name: "nokey"}
        ]
      }

      resolve = fn query -> {:ok, Map.get(entries, query, [])} end
      %{bob: bob, nokey: nokey, me: me, context: %{resolve: resolve, identity: me}}
    end

    test "pubkey renders the resolved hex key", %{bob: bob, context: ctx} do
      values = %{"to" => Arc.Identity.name(bob)}

      assert {:ok, "send " <> hex} = Toolbox.render_template("send {{to|pubkey}}", values, ctx)
      assert hex == Arc.Identity.encode_public_key(bob)
    end

    test "pubkey passes a full hex key through without a resolver", %{bob: bob} do
      hex = Arc.Identity.encode_public_key(bob)
      assert {:ok, ^hex} = Toolbox.render_template("{{to|pubkey}}", %{"to" => hex}, %{})
    end

    test "pubkey fails when the name does not resolve", %{context: ctx} do
      assert {:error, {:resolve, "nobody", :not_found}} =
               Toolbox.render_template("{{to|pubkey}}", %{"to" => "nobody"}, ctx)
    end

    test "seal:to produces a token only the target can open", %{bob: bob, me: me, context: ctx} do
      values = %{"to" => Arc.Identity.name(bob), "body" => "hi bob"}

      assert {:ok, "sealed-v1:" <> _ = token} =
               Toolbox.render_template("{{body|seal:to}}", values, ctx)

      assert Toolbox.open_tokens("x " <> token <> " y", bob) == "x hi bob y"
      assert Toolbox.open_tokens(token, me) == "[sealed: cannot open]"
    end

    test "seal:me seals to the caller", %{me: me, context: ctx} do
      assert {:ok, token} = Toolbox.render_template("{{body|seal:me}}", %{"body" => "note"}, ctx)
      assert Toolbox.open_tokens(token, me) == "note"
    end

    test "seal fails when the target has no keyex", %{nokey: nokey, context: ctx} do
      name = Arc.Identity.name(nokey)

      assert {:error, {:no_keyex, ^name}} =
               Toolbox.render_template("{{body|seal:to}}", %{"to" => name, "body" => "x"}, ctx)
    end

    test "seal without a target is a template error" do
      assert {:error, {:invalid_template, _}} =
               Toolbox.render_template("{{body|seal}}", %{"body" => "x"}, %{})
    end

    test "seal_to/4 seals a body through the same path", %{bob: bob, context: ctx} do
      values = %{"to" => Arc.Identity.name(bob)}
      assert {:ok, token} = Toolbox.seal_to("stdin body", "to", values, ctx)
      assert Toolbox.open_tokens(token, bob) == "stdin body"
    end

    test "petnames replaces hex public keys with petnames", %{bob: bob} do
      hex = Arc.Identity.encode_public_key(bob)
      name = Arc.Identity.name(bob)
      assert Toolbox.petnames("id\t#{hex}\tx #{hex}.") == "id\t#{name}\tx #{name}."
      assert Toolbox.petnames("abc") == "abc"
    end

    test "preview truncates the last tab field to one line" do
      long = String.duplicate("word ", 30)
      out = Toolbox.preview("header line\nid\tin\t#{long}", 12)
      assert out == "header line\nid\tin\tword word w…"
      assert Toolbox.preview("a\tb", 5) == "a\tb"
    end

    test "preview folds an opened multi-line body into its record" do
      out =
        Toolbox.preview(
          "3 unread\nid\tin\t# Review\n\nSection 3 needs a cap.\n\nid2\tin\tshort",
          20
        )

      assert out == "3 unread\nid\tin\t# Review Section 3 …\nid2\tin\tshort"
    end

    test "apply_output_filters chains in order", %{bob: bob, context: ctx} do
      {:ok, token} =
        Toolbox.render_template(
          "{{body|seal:to}}",
          %{"to" => Arc.Identity.name(bob), "body" => "hello there friend"},
          ctx
        )

      hex = Arc.Identity.encode_public_key(bob)
      text = "#{hex}\t#{token}"

      assert Toolbox.apply_output_filters(text, ["open", "petnames", "preview:8"], bob) ==
               "#{Arc.Identity.name(bob)}\thello t…"

      assert Toolbox.apply_output_filters(text, ["open"], nil) == text
    end

    test "conversation renders thread --bodies records" do
      text =
        Enum.join(
          [
            "jolly-volta - 2 messages, 1 unread",
            "01AAAAAAAAAAAAAAAAAAAAAAAA\tin\tjolly-volta\t2026-09-14T19:22:50Z\t-\tunread\tfirst line",
            "second line",
            "01BBBBBBBBBBBBBBBBBBBBBBBB\tout\tjolly-volta\t2026-09-14T19:23:00Z\t01AAAAAAAAAAAAAAAAAAAAAAAA\tread\tyes"
          ],
          "\n"
        )

      assert Toolbox.apply_output_filters(text, ["conversation"], nil) ==
               Enum.join(
                 [
                   "── jolly-volta - 2 messages, 1 unread ──",
                   "",
                   "jolly-volta  2026-09-14 19:22  AAAAAA",
                   "  first line",
                   "  second line",
                   "",
                   "you  2026-09-14 19:23  BBBBBB  ↳ reply to AAAAAA",
                   "  yes",
                   "  ✓ read"
                 ],
                 "\n"
               )
    end

    test "render_template/2 still works without a context" do
      assert {:ok, ~s(a "b")} =
               Toolbox.render_template("{{x}} {{y|json}}", %{"x" => "a", "y" => "b"})
    end
  end

  test "json source sends every parsed argument as one JSON object" do
    tool = tool(%{"source" => "json"}, @args)

    assert {:ok, %{input: input}} =
             Toolbox.build_invocation(tool, [
               "write",
               "inbox",
               "--body",
               ~s(He said "hi"),
               "--draft"
             ])

    assert :json.decode(input) == %{
             "addr" => "inbox",
             "body" => ~s(He said "hi"),
             "draft" => true
           }
  end
end
