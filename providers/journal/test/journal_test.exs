defmodule JournalTest do
  use ExUnit.Case

  alias Journal.{Command, Parse}

  @alice String.duplicate("a1", 16)
  @bob String.duplicate("b2", 16)

  setup do
    root = Path.join(System.tmp_dir!(), "journal-test-#{System.unique_integer([:positive])}")
    :ok = Journal.Store.ensure(root)
    on_exit(fn -> File.rm_rf!(root) end)
    {:ok, root: root}
  end

  defp run(root, from, line), do: Command.run(root, from, line)
  defp encode_json(value), do: value |> :json.encode() |> IO.iodata_to_binary()
  defp json(value), do: encode_json(value)

  test "parse handles quotes inside option values" do
    {args, opts} =
      Parse.parse(~s(write hrs/nb/p --body "He said "hi" today" --title "T" --if-rev ""))

    assert args == ["write", "hrs/nb/p"]
    assert opts["body"] == ~s(He said "hi" today)
    assert opts["title"] == "T"
    refute Map.has_key?(opts, "if_rev")
  end

  test "parse decodes JSON string option values" do
    line =
      ~s(write hrs/ab/p1 --body "He said \\"hi\\" --not-a-flag\\nline two" ) <>
        ~s(--title "P \\"one\\"" --tags "" --if-rev "")

    {args, opts} = Parse.parse(line)

    assert args == ["write", "hrs/ab/p1"]
    assert opts["body"] == "He said \"hi\" --not-a-flag\nline two"
    assert opts["title"] == ~s(P "one")
    refute Map.has_key?(opts, "tags")
    refute Map.has_key?(opts, "if_rev")
  end

  test "parse treats a bare null and an empty JSON string as absent" do
    {_, opts} = Parse.parse(~s(write hrs/ab/p1 --body "x" --title null --tags "" --note ""))
    assert opts == %{"body" => "x"}
  end

  test "parse decodes JSON string options and skips bare null" do
    title = ~s(P "one" --not-a-flag\nline two)
    header = "write hrs/nb/p --title #{json(title)} --tags null --if-rev null"

    assert {["write", "hrs/nb/p"], %{"title" => ^title}} = Parse.parse(header)

    assert {["read", "hrs/nb/p"], %{"lines" => "1:40"}} =
             Parse.parse("read hrs/nb/p --lines 1:40")

    assert {_, %{"body" => "a\\nb", "title" => "null"}} =
             Parse.parse(~s(x --body "a\\\\nb" --title "null"))
  end

  test "parse decodes JSON positionals and keeps a flag-like word inside them" do
    text = ~s(tried --lr 3e-4, "worse"\nline two)
    line = "append hrs/ab/p1 #{json(text)}"
    assert {["append", "hrs/ab/p1", ^text], %{}} = Parse.parse(line)

    query = "loss --deep dive"
    line = "search #{json(query)} --project hrs --deep"
    assert {["search", ^query], %{"project" => "hrs", "deep" => true}} = Parse.parse(line)
  end

  test "parse bare flags and positional quotes" do
    {args, opts} = Parse.parse(~s(search "learning rate" --project hrs --deep))
    assert args == ["search", "learning rate"]
    assert opts == %{"project" => "hrs", "deep" => true}
  end

  test "parse treats a template-rendered boolean like a bare flag" do
    line = ~s(search "learning rate" --project "" --notebook "" --deep "true")
    assert {["search", "learning rate"], %{"deep" => true}} = Parse.parse(line)

    assert {["a"], %{"x" => true}} = Parse.parse("a --x true")
    assert {["a"], %{}} = Parse.parse(~s(a --x "false"))
  end

  test "parse keeps a multi-megabyte quoted value and the flags after it" do
    big = String.duplicate("A", 8 * 1024 * 1024)
    {args, opts} = Parse.parse(~s(attach hrs/ab/p1 --base64 "#{big}" --name big.bin --deep))
    assert args == ["attach", "hrs/ab/p1"]
    assert opts["base64"] == big
    assert opts["name"] == "big.bin"
    assert opts["deep"] == true
  end

  test "parse edge cases match the documented rules" do
    assert {["a"], %{"x" => true, "y" => "1"}} = Parse.parse("a --x --y 1")
    assert {["a"], %{"x" => "1"}} = Parse.parse("a --x 1 junk --y false")
    assert {["a"], %{"x" => ~s("q")}} = Parse.parse(~s(a --x "q" tail))
    assert {["a"], %{"x" => "no close"}} = Parse.parse(~s(a --x "no close"))

    assert {["a"], %{"x" => ~s("unterminated), "y" => "2"}} =
             Parse.parse(~s(a --x "unterminated --y 2))

    assert {["a"], %{"body" => "l1\nl2", "t" => "T"}} = Parse.parse("a --body \"l1\nl2\" --t T")
    assert {["a"], %{"if_rev" => "r"}} = Parse.parse("a --if-rev r")
  end

  test "write, read, ls, rev and conflict", %{root: root} do
    assert {:ok, "rev: " <> rev} =
             run(root, @alice, ~s(write hrs/ab/p1 --title "P one" --body "# Hello\\n\\nworld"))

    assert {:ok, out} = run(root, @alice, "read hrs/ab/p1")
    assert out =~ "rev: #{rev}"
    assert out =~ ~s(title: "P one")
    assert out =~ "author: #{@alice}"
    assert out =~ "# Hello\n\nworld"

    assert {:ok, "hrs/ab/p1\tP one"} = run(root, @alice, "ls hrs/ab")
    assert {:ok, "hrs"} = run(root, @alice, "ls")

    assert {:error, "conflict" <> _} =
             run(root, @alice, ~s(write hrs/ab/p1 --body "x" --if-rev deadbee))

    assert {:ok, _} = run(root, @alice, ~s(write hrs/ab/p1 --body "x" --if-rev #{rev}))
  end

  test "write stores a body with a flag-like word, quotes and a newline", %{root: root} do
    body = "He said \"hi\" --not-a-flag\nline two"
    line = ~s(write hrs/ab/p1 --title "P \\"one\\"" --body ) <> encode_json(body)

    assert {:ok, "rev: " <> _} = run(root, @alice, line)
    assert {:ok, out} = run(root, @alice, "read hrs/ab/p1")
    assert out =~ body
    assert {:ok, ~s(hrs/ab/p1\tP "one")} == run(root, @alice, "ls hrs/ab")
  end

  test "write takes a 2 MB body from the request body", %{root: root} do
    line = ~s(x --tags y "q" \\n ) <> String.duplicate("a", 100) <> "\n"
    body = String.duplicate(line, div(2 * 1024 * 1024, byte_size(line)) + 1)
    assert byte_size(body) > 2 * 1024 * 1024

    header = ~s(write hrs/ab/big --title "Big" --tags null --if-rev null)
    assert {:ok, "rev: " <> rev} = run(root, @alice, header <> "\n" <> body)

    {:ok, out} = run(root, @alice, "read hrs/ab/big")
    assert String.starts_with?(out, "rev: #{rev}\n")
    assert out =~ "title: Big\n"
    refute out =~ "tags:"
    assert String.ends_with?(out, "---\n" <> body)

    assert {:ok, "rev: " <> _} = run(root, @alice, "write hrs/ab/big --if-rev #{rev}\nno newline")
    {:ok, out} = run(root, @alice, "read hrs/ab/big")
    assert String.ends_with?(out, "---\nno newline\n")

    assert {:ok, _} = run(root, @alice, "write hrs/ab/big\n")
    {:ok, out} = run(root, @alice, "read hrs/ab/big")
    assert String.ends_with?(out, "---\n")
  end

  test "write needs exactly one body", %{root: root} do
    assert {:error, "missing body" <> _} = run(root, @alice, ~s(write hrs/ab/p1 --title "T"))
    assert {:error, "missing body" <> _} = run(root, @alice, "write hrs/ab/p1 --body")

    assert {:error, "invalid_arguments" <> _} =
             run(root, @alice, ~s(write hrs/ab/p1 --body "x"\ny))

    assert {:ok, _} = run(root, @alice, ~s(write hrs/ab/p1 --body "x"\n))
    {:ok, out} = run(root, @alice, "read hrs/ab/p1")
    assert String.ends_with?(out, "---\nx\n")
  end

  test "acl: creator owns, others forbidden until added", %{root: root} do
    {:ok, _} = run(root, @alice, ~s(write hrs/ab/p1 --body "x"))
    assert {:error, "forbidden"} = run(root, @bob, "read hrs/ab/p1")
    assert {:error, "forbidden"} = run(root, @bob, ~s(write hrs/ab/p2 --body "y"))
    assert {:error, "forbidden" <> _} = run(root, @bob, "acl hrs add #{@bob}")
    assert {:ok, _} = run(root, @alice, "acl hrs add #{@bob}")
    assert {:ok, _} = run(root, @bob, "read hrs/ab/p1")
    assert {:ok, "hrs"} = run(root, @bob, "ls")
  end

  test "append, edit and history", %{root: root} do
    {:ok, "rev: " <> _} = run(root, @alice, ~s(write hrs/ab/p1 --body "line one"))
    {:ok, "rev: " <> rev} = run(root, @alice, ~s(append hrs/ab/p1 "line two, quoted"))
    {:ok, out} = run(root, @alice, "read hrs/ab/p1")
    assert out =~ "line one\n\nline two, quoted\n"

    assert {:error, "missing --if-rev"} =
             run(root, @alice, ~s(edit hrs/ab/p1 --find one --replace uno))

    {:ok, "rev: " <> _} =
      run(root, @alice, ~s(edit hrs/ab/p1 --if-rev #{rev} --find "line one" --replace "line uno"))

    {:ok, out} = run(root, @alice, "read hrs/ab/p1")
    assert out =~ "line uno"

    {:ok, hist} = run(root, @alice, "history hrs/ab/p1")
    assert length(String.split(hist, "\n")) == 3
    assert hist =~ String.slice(@alice, 0, 12)
  end

  test "append stores text with a flag-like word and a newline", %{root: root} do
    {:ok, _} = run(root, @alice, ~s(write hrs/ab/p1 --body "base"))
    text = "tried --lr 3e-4, worse\nsecond line"
    {:ok, "rev: " <> _} = run(root, @alice, "append hrs/ab/p1 #{json(text)}")
    {:ok, out} = run(root, @alice, "read hrs/ab/p1")
    assert String.ends_with?(out, "---\nbase\n\n" <> text <> "\n")
  end

  test "kpi set, log, latest and page injection", %{root: root} do
    {:ok, _} = run(root, @alice, ~s(write hrs/ab/p1 --body "AUC: <!-- kpi: auc -->"))
    {:ok, _} = run(root, @alice, ~s(kpi set hrs/ab auc 0.85 --ref abc123))
    {:ok, _} = run(root, @alice, ~s(kpi set hrs/ab auc 0.871 --note "lr=3e-4"))
    {:ok, _} = run(root, @alice, ~s(kpi set hrs/ab loss 12))

    {:ok, log} = run(root, @alice, "kpi log hrs/ab auc")
    assert [l1, l2] = String.split(log, "\n")
    assert l1 =~ "auc=0.85" and l1 =~ "ref=abc123"
    assert l2 =~ "auc=0.871" and l2 =~ "note=lr=3e-4"

    {:ok, latest} = run(root, @alice, "kpi latest hrs/ab")
    assert latest =~ "auc=0.871"
    assert latest =~ "loss=12"

    {:ok, page} = run(root, @alice, "read hrs/ab/p1")
    assert page =~ "AUC: auc = 0.871 ("
    assert {:error, "invalid_number"} = run(root, @alice, "kpi set hrs/ab auc high")
  end

  test "attach, fetch, link", %{root: root} do
    {:ok, _} = run(root, @alice, ~s(write hrs/ab/p1 --body "x"))
    b64 = Base.encode64("hello bytes")
    {:ok, out} = run(root, @alice, ~s(attach hrs/ab/p1 --name hello.txt --base64 "#{b64}"))
    [_, "sha256: " <> sha] = String.split(out, "\n")
    assert {:ok, ^b64} = run(root, @alice, "fetch #{sha}")

    {:ok, _} = run(root, @alice, ~s(link hrs/ab/p1 file:///Users/zim/runs/x --name run-dir))
    {:ok, _} = run(root, @alice, ~s(link hrs/ab/p1 s3://bucket/ckpt.pt --sha256 abc))
    {:ok, page} = run(root, @alice, "read hrs/ab/p1")
    assert page =~ "name: hello.txt"
    assert page =~ "sha256: #{sha}"
    assert page =~ "uri: file:///Users/zim/runs/x"
    assert page =~ "host: #{@alice}"
    assert page =~ "uri: s3://bucket/ckpt.pt"

    big = Base.encode64(:binary.copy(<<0>>, Journal.Config.max_blob_bytes() + 1))

    assert {:error, "too_large" <> _} =
             run(root, @alice, ~s(attach hrs/ab/p1 --name big --base64 "#{big}"))
  end

  test "attach and fetch a 2 MiB blob on one reply line", %{root: root} do
    import ExUnit.CaptureIO

    {:ok, _} = run(root, @alice, ~s(write hrs/ab/p1 --body "x"))
    bytes = :crypto.strong_rand_bytes(2 * 1024 * 1024)
    b64 = Base.encode64(bytes)

    {:ok, out} = run(root, @alice, ~s(attach hrs/ab/p1 --name big.bin --base64 "#{b64}"))
    [_, "sha256: " <> sha] = String.split(out, "\n")
    assert sha == Base.encode16(:crypto.hash(:sha256, bytes), case: :lower)
    assert {:ok, ^b64} = run(root, @alice, "fetch #{sha}")

    {:ok, page} = run(root, @alice, "read hrs/ab/p1")
    assert page =~ "name: big.bin"
    assert page =~ "bytes: #{byte_size(bytes)}"

    # The reply is a single stdout line larger than the 1 MB cap the ARC exec
    # port used to have. Nothing between the journal and the port splits it.
    out =
      capture_io(fn ->
        Journal.Stdio.handle_line(
          root,
          ~s({"op":"request","message":"fetch #{sha}","from":"#{@alice}","request_id":"r1"})
        )
      end)

    assert byte_size(out) > 1_000_000
    assert String.ends_with?(out, "\n")
    line = String.trim_trailing(out, "\n")
    refute line =~ "\n"
    assert %{"op" => "reply", "request_id" => "r1", "reply" => ^b64} = :json.decode(line)
  end

  test "stores a literal backslash-n unchanged", %{root: root} do
    body = ~S(path C:\new\table)
    {:ok, "rev: " <> rev} = run(root, @alice, "write hrs/ab/p2 --body #{:json.encode(body)}")
    {:ok, out} = run(root, @alice, "read hrs/ab/p2")
    assert out =~ body
    refute out =~ "C:\new"

    edit =
      "edit hrs/ab/p2 --if-rev #{rev} --find #{:json.encode(~S(\new))} " <>
        "--replace #{:json.encode(~S(\old))}"

    {:ok, _} = run(root, @alice, edit)
    {:ok, out} = run(root, @alice, "read hrs/ab/p2")
    assert out =~ ~S(path C:\old\table)

    {:ok, _} = run(root, @alice, "append hrs/ab/p2 #{:json.encode(~S(re \d+\n))}")
    {:ok, out} = run(root, @alice, "read hrs/ab/p2")
    assert out =~ ~S(re \d+\n)
  end

  test "read with line range", %{root: root} do
    {:ok, _} = run(root, @alice, ~s(write hrs/ab/p1 --body "a\\nb\\nc\\nd"))
    {:ok, out} = run(root, @alice, "read hrs/ab/p1 --lines 6:7")
    assert out =~ ~r/^rev: [0-9a-f]+\n# ?a\nb$/ or out =~ ~r/^rev: [0-9a-f]+\na\nb$/
  end

  test "rejects bad addresses and missing key", %{root: root} do
    assert {:error, "invalid_address"} = run(root, @alice, ~s(write hrs/ab --body x))
    assert {:error, "invalid_address"} = run(root, @alice, ~s(write ../x/y --body x))
    assert {:error, "forbidden" <> _} = run(root, "", "ls")
  end

  test "stdio protocol round trip", %{root: root} do
    import ExUnit.CaptureIO

    out =
      capture_io(fn ->
        Journal.Stdio.handle_line(
          root,
          ~s({"op":"request","message":"write hrs/ab/p1 --body \\"hi\\"","from":"#{@alice}","request_id":"r1"})
        )

        Journal.Stdio.handle_line(
          root,
          ~s({"op":"request","message":"read hrs/ab/p1","from":"#{@bob}","request_id":"r2"})
        )

        message =
          ~s(write hrs/ab/p2 --title "T \\"two\\"" --tags null --if-rev null\n# Two\n\nbody\n)

        Journal.Stdio.handle_line(
          root,
          json(%{"op" => "request", "message" => message, "from" => @alice, "request_id" => "r3"})
        )

        Journal.Stdio.handle_line(
          root,
          ~s({"op":"request","message":"read hrs/ab/p2","from":"#{@alice}","request_id":"r4"})
        )
      end)

    [l1, l2, l3, l4] = String.split(String.trim(out), "\n")
    assert %{"op" => "reply", "request_id" => "r1", "reply" => "rev: " <> _} = :json.decode(l1)

    assert %{"op" => "error", "request_id" => "r2", "error" => "forbidden"} =
             :json.decode(l2)

    assert %{"op" => "reply", "request_id" => "r3", "reply" => "rev: " <> _} = :json.decode(l3)
    assert %{"op" => "reply", "request_id" => "r4", "reply" => page} = :json.decode(l4)
    assert page =~ ~s(title: "T \\"two\\"")
    assert String.ends_with?(page, "---\n# Two\n\nbody\n")
  end
end
