defmodule ReleasesTest do
  use ExUnit.Case, async: true

  alias Releases.{Config, Store}
  import ExUnit.CaptureIO

  setup do
    root = Path.join(System.tmp_dir!(), "arc-releases-#{System.unique_integer([:positive])}")
    File.mkdir_p!(Path.join(root, "channels"))
    File.mkdir_p!(Path.join(root, "blobs"))
    on_exit(fn -> File.rm_rf!(root) end)
    %{root: root}
  end

  test "reads fixed channel documents and bounded immutable chunks", %{root: root} do
    File.write!(Path.join([root, "channels", "stable.json"]), "{\"sequence\":1}")
    digest = String.duplicate("a", 64)
    File.write!(Path.join([root, "blobs", digest <> ".tar.gz"]), "abcdef")

    assert {:ok, "{\"sequence\":1}"} = Store.channel(root, "stable")

    assert {:ok, %{"digest" => "sha256:" <> ^digest, "offset" => 2, "data" => data}} =
             Store.chunk(root, "sha256:" <> digest, 2, 3)

    assert Base.decode64!(data) == "cde"
    assert {:ok, %{"data" => ""}} = Store.chunk(root, "sha256:" <> digest, 6, 1)
  end

  test "rejects arbitrary names, symlinks, and oversized chunks", %{root: root} do
    digest = String.duplicate("b", 64)
    File.write!(Path.join([root, "blobs", digest <> ".tar.gz"]), "bytes")

    File.ln_s!(
      Path.join([root, "blobs", digest <> ".tar.gz"]),
      Path.join([root, "blobs", String.duplicate("c", 64) <> ".tar.gz"])
    )

    assert {:error, "invalid_request"} = Store.channel(root, "../stable")
    assert {:error, "invalid_request"} = Store.chunk(root, "sha256:../x", 0, 1)

    assert {:error, "storage_failure"} =
             Store.chunk(root, "sha256:" <> String.duplicate("c", 64), 0, 1)

    assert {:error, "invalid_request"} =
             Store.chunk(root, "sha256:" <> digest, 0, Config.max_chunk_bytes() + 1)
  end

  test "stdio emits the chunk wire schema", %{root: root} do
    digest = String.duplicate("d", 64)
    File.write!(Path.join([root, "blobs", digest <> ".tar.gz"]), "abc")

    message =
      :json.encode(%{
        "op" => "chunk",
        "digest" => "sha256:" <> digest,
        "offset" => 0,
        "length" => 3
      })
      |> IO.iodata_to_binary()

    line =
      :json.encode(%{
        "op" => "request",
        "request_id" => "request-1",
        "message" => message
      })
      |> IO.iodata_to_binary()

    output = capture_io(fn -> Releases.Stdio.handle_line(root, line <> "\n") end)

    assert %{"op" => "reply", "request_id" => "request-1", "reply" => reply} =
             :json.decode(output)

    assert reply["digest"] == "sha256:" <> digest
    assert reply["offset"] == 0
    assert Base.decode64!(reply["data"]) == "abc"
  end
end
