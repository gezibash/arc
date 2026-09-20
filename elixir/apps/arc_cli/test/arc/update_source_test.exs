defmodule Arc.CLI.Update.SourceTest do
  use ExUnit.Case, async: true

  alias Arc.CLI.Update.Source

  setup do
    root = Path.join(System.tmp_dir!(), "arc-update-source-#{System.unique_integer([:positive])}")
    File.mkdir_p!(Path.join(root, "channels"))
    File.mkdir_p!(Path.join(root, "blobs"))
    destination = Path.join(root, "staging")
    File.mkdir_p!(destination)
    on_exit(fn -> File.rm_rf!(root) end)
    %{root: root, destination: destination}
  end

  test "reads a bounded local channel and stages a verified archive", ctx do
    bytes = :crypto.strong_rand_bytes(300_000)
    digest = :crypto.hash(:sha256, bytes) |> Base.encode16(case: :lower)
    File.write!(Path.join([ctx.root, "channels", "stable.json"]), "{\"channel\":\"stable\"}")
    File.write!(Path.join([ctx.root, "blobs", digest <> ".tar.gz"]), bytes)

    assert {:ok, "{\"channel\":\"stable\"}"} = Source.channel({:local, ctx.root}, "stable")

    destination = Path.join(ctx.destination, digest <> ".tar.gz")

    assert {:ok, ^destination} =
             Source.stage({:local, ctx.root}, digest, byte_size(bytes), destination)

    assert File.read!(destination) == bytes

    assert {:error, :destination_exists} =
             Source.stage({:local, ctx.root}, digest, byte_size(bytes), destination)
  end

  test "never commits a digest mismatch or follows source links", ctx do
    bytes = "release bytes"
    digest = String.duplicate("0", 64)
    source = Path.join([ctx.root, "blobs", digest <> ".tar.gz"])
    File.write!(source, bytes)
    destination = Path.join(ctx.destination, "candidate.tar.gz")

    assert {:error, :digest_mismatch} =
             Source.stage({:local, ctx.root}, digest, byte_size(bytes), destination)

    refute File.exists?(destination)
    assert File.ls!(ctx.destination) == []

    real_digest = :crypto.hash(:sha256, bytes) |> Base.encode16(case: :lower)
    File.ln_s!(source, Path.join([ctx.root, "blobs", real_digest <> ".tar.gz"]))

    assert {:error, :source_read_failed} =
             Source.stage({:local, ctx.root}, real_digest, byte_size(bytes), destination)

    refute File.exists?(destination)
  end

  test "rejects destination directories outside caller prepared staging", ctx do
    digest = String.duplicate("a", 64)

    assert {:error, :invalid_destination} =
             Source.stage(
               {:local, ctx.root},
               digest,
               0,
               Path.join(ctx.root, "missing/candidate.tar.gz")
             )
  end
end
