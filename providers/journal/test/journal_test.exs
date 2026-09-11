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

  test "parse handles quotes inside option values" do
    {args, opts} =
      Parse.parse(~s(write hrs/nb/p --body "He said "hi" today" --title "T" --if-rev ""))

    assert args == ["write", "hrs/nb/p"]
    assert opts["body"] == ~s(He said "hi" today)
    assert opts["title"] == "T"
    refute Map.has_key?(opts, "if_rev")
  end

  test "parse bare flags and positional quotes" do
    {args, opts} = Parse.parse(~s(search "learning rate" --project hrs --deep))
    assert args == ["search", "learning rate"]
    assert opts == %{"project" => "hrs", "deep" => true}
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
      end)

    [l1, l2] = String.split(String.trim(out), "\n")
    assert %{"op" => "reply", "request_id" => "r1", "reply" => "rev: " <> _} = :json.decode(l1)

    assert %{"op" => "error", "request_id" => "r2", "error" => "forbidden"} =
             :json.decode(l2)
  end
end
