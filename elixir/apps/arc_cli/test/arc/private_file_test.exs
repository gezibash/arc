defmodule Arc.CLI.PrivateFileTest do
  use ExUnit.Case, async: true

  alias Arc.CLI.PrivateFile
  alias Arc.Identity
  alias Arc.Identity.SealedBox

  setup do
    root = Path.join(System.tmp_dir!(), "arc-private-file-#{System.unique_integer([:positive])}")
    File.mkdir_p!(root)
    on_exit(fn -> File.rm_rf!(root) end)
    %{root: root, alice: Identity.generate(), bob: Identity.generate()}
  end

  defp json(value), do: value |> :json.encode() |> IO.iodata_to_binary()

  defp get(file, destination, identity, requested_id \\ nil) do
    PrivateFile.finish(
      json(%{"file" => file}),
      %{
        input: json(%{"op" => "get", "id" => requested_id || file["id"]}),
        values: %{"output" => destination}
      },
      %{"operation" => "get", "path" => "output"},
      identity
    )
  end

  test "binary contents and private basename are sealed locally; download is byte exact", ctx do
    name = "private finance 日本語.bin"
    path = Path.join(ctx.root, name)
    bytes = <<0, 255, 128, 10, 13>> <> "private payload without final newline"
    File.write!(path, bytes)
    assert {:ok, input} = PrivateFile.upload(path, ctx.alice)
    refute input =~ name
    refute input =~ "private payload"
    refute input =~ ctx.root
    %{"op" => "put", "file" => file} = :json.decode(input)
    assert {:ok, ^name} = PrivateFile.verify_header(file, ctx.alice)
    assert {:error, _} = PrivateFile.verify_header(file, ctx.bob)
    "sealed-v1:" <> encoded = file["body"]
    assert {:error, :open_failed} = SealedBox.open(ctx.bob, Base.decode64!(encoded))
    out = Path.join(ctx.root, "restored.bin")
    assert {:ok, _} = get(file, out, ctx.alice)
    assert File.read!(out) == bytes
    assert Bitwise.band(File.stat!(out).mode, 0o777) == 0o600
  end

  test "empty files round trip", ctx do
    source = Path.join(ctx.root, "empty")
    File.write!(source, "")
    assert {:ok, input} = PrivateFile.upload(source, ctx.alice)
    out = Path.join(ctx.root, "restored")
    assert {:ok, _} = get(:json.decode(input)["file"], out, ctx.alice)
    assert File.read!(out) == ""
  end

  test "oversized and nonregular uploads are rejected", ctx do
    source = Path.join(ctx.root, "large")
    File.write!(source, :binary.copy(<<0>>, 4 * 1024 * 1024 + 1))
    assert {:error, _} = PrivateFile.upload(source, ctx.alice)
    assert {:error, _} = PrivateFile.upload(ctx.root, ctx.alice)
    assert {:error, _} = PrivateFile.upload(nil, ctx.alice)
  end

  test "tampering and substituting a different valid file never creates output", ctx do
    file = PrivateFile.seal("original", "important", ctx.alice)
    another = PrivateFile.seal("another", "different", ctx.alice)
    out = Path.join(ctx.root, "output")

    for bad <- [
          Map.put(file, "body", another["body"]),
          Map.put(file, "name", another["name"]),
          Map.put(file, "signature", String.duplicate("0", 128))
        ] do
      assert {:error, _} = get(bad, out, ctx.alice)
      refute File.exists?(out)
    end

    assert {:error, _} = get(another, out, ctx.alice, file["id"])
    assert {:error, _} = get(file, out, ctx.bob)
    refute File.exists?(out)
  end

  test "existing paths and symlinks are not overwritten", ctx do
    file = PrivateFile.seal("../../server-chosen-path", "secret", ctx.alice)
    out = Path.join(ctx.root, "existing")
    File.write!(out, "keep")
    assert {:error, _} = get(file, out, ctx.alice)
    link = Path.join(ctx.root, "link")
    File.ln_s!(out, link)
    assert {:error, _} = get(file, link, ctx.alice)
    assert File.read!(out) == "keep"
    assert {:error, _} = get(file, nil, ctx.alice)
  end

  test "a signed malformed encryption key fails without crashing or creating output", ctx do
    file = PrivateFile.seal("original", "data", ctx.alice)
    body = "sealed-v1:" <> Base.encode64(<<1, 0::256, 0::128>>)
    hash = :crypto.hash(:sha256, body) |> Base.encode16(case: :lower)
    message = "arc-private-file-v1\n#{file["owner"]}\n#{file["name"]}\n#{hash}"
    signature = Identity.sign(ctx.alice, message) |> Base.encode16(case: :lower)
    id = :crypto.hash(:sha256, message <> "\n" <> signature) |> Base.encode16(case: :lower)

    malformed =
      Map.merge(file, %{"body" => body, "body_hash" => hash, "signature" => signature, "id" => id})

    out = Path.join(ctx.root, "malformed")
    assert {:error, "file decryption failed"} = get(malformed, out, ctx.alice)
    refute File.exists?(out)
  end

  test "download sends only the ID, never a local destination", ctx do
    id = String.duplicate("a", 64)
    values = %{"id" => id, "output" => Path.join(ctx.root, "private path")}

    assert {:ok, input} =
             PrivateFile.request("get", values, %{"id" => "id"}, %{"path" => "output"})

    assert :json.decode(input) == %{"op" => "get", "id" => id}

    assert {:error, _} =
             PrivateFile.request("get", %{"id" => id}, %{"id" => "id"}, %{"path" => "output"})

    assert {:error, _} =
             PrivateFile.request("list", %{"after" => "../"}, %{"after" => "after"}, %{})
  end

  test "put acknowledgements must match the uploaded object", ctx do
    file = PrivateFile.seal("name", "body", ctx.alice)
    built = %{input: json(%{"op" => "put", "file" => file}), values: %{}}
    spec = %{"operation" => "put"}
    assert {:ok, id} = PrivateFile.finish(json(%{"id" => file["id"]}), built, spec, ctx.alice)
    assert id == file["id"]

    assert {:error, _} =
             PrivateFile.finish(
               json(%{"id" => String.duplicate("0", 64)}),
               built,
               spec,
               ctx.alice
             )
  end

  test "list verifies ownership and escapes filename control characters", ctx do
    file = PrivateFile.seal("secret\n\e[31m.bin", "data", ctx.alice)
    header = Map.delete(file, "body")
    built = %{input: json(%{"op" => "list"}), values: %{}}
    spec = %{"operation" => "list"}
    reply = json(%{"files" => [header], "next" => :null})
    assert {:ok, text} = PrivateFile.finish(reply, built, spec, ctx.alice)
    assert text =~ "secret\\n"
    refute text =~ <<27>>
    assert {:error, _} = PrivateFile.finish(reply, built, spec, ctx.bob)
    assert {:error, _} = PrivateFile.finish("not json", built, spec, ctx.alice)

    assert {:error, _} =
             PrivateFile.finish(
               json(%{"files" => [nil], "next" => :null}),
               built,
               spec,
               ctx.alice
             )
  end
end
