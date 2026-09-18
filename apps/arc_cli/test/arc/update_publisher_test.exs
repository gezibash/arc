defmodule Arc.CLI.Update.PublisherTest do
  use ExUnit.Case, async: true

  alias Arc.CLI.Update.Manifest
  alias Arc.CLI.Update.Publisher
  alias Arc.Identity

  setup do
    root =
      Path.join(System.tmp_dir!(), "arc-update-publisher-#{System.unique_integer([:positive])}")

    File.mkdir_p!(Path.join(root, "channels"))
    File.mkdir_p!(Path.join(root, "blobs"))
    on_exit(fn -> File.rm_rf!(root) end)
    %{root: root, publisher: Identity.generate()}
  end

  test "signs and atomically publishes a verified channel document", %{
    root: root,
    publisher: publisher
  } do
    unsigned = release_manifest(root, publisher, 1, "build-001", "archive one")

    assert :ok = Publisher.publish(root, publisher, unsigned)

    path = Path.join([root, "channels", "stable.json"])
    signed = path |> File.read!() |> :json.decode()

    assert {:ok, verified} =
             Manifest.verify(signed,
               expected_publisher: Identity.encode_public_key(publisher),
               expected_channel: "stable",
               now: System.system_time(:second)
             )

    assert verified.manifest == unsigned
    assert File.ls!(Path.join(root, "channels")) == ["stable.json"]
  end

  test "refuses a blob whose bytes no longer match its signed digest", %{
    root: root,
    publisher: publisher
  } do
    unsigned = release_manifest(root, publisher, 1, "build-001", "archive one")
    [release] = unsigned["releases"]
    blob = Path.join([root, "blobs", release["sha256"] <> ".tar.gz"])
    File.write!(blob, "archive two")

    assert {:error, :archive_digest_mismatch} = Publisher.publish(root, publisher, unsigned)
    refute File.exists?(Path.join([root, "channels", "stable.json"]))
  end

  test "refuses a release whose install archive is absent from the blob store", %{
    root: root,
    publisher: publisher
  } do
    unsigned = release_manifest(root, publisher, 1, "build-001", "archive one")
    install = %{"sha256" => String.duplicate("c", 64), "size" => 11}
    unsigned = Map.update!(unsigned, "releases", fn [r] -> [Map.put(r, "install", install)] end)

    assert {:error, :enoent} = Publisher.publish(root, publisher, unsigned)
    refute File.exists?(Path.join([root, "channels", "stable.json"]))
  end

  test "requires a strictly increasing sequence after publication", %{
    root: root,
    publisher: publisher
  } do
    first = release_manifest(root, publisher, 1, "build-001", "archive one")
    assert :ok = Publisher.publish(root, publisher, first)

    repeated = release_manifest(root, publisher, 1, "build-002", "archive two")

    assert {:error, :sequence_not_increased} = Publisher.publish(root, publisher, repeated)
  end

  test "keeps immutable release build metadata after publication", %{
    root: root,
    publisher: publisher
  } do
    first = release_manifest(root, publisher, 1, "build-001", "archive one")
    assert :ok = Publisher.publish(root, publisher, first)

    changed = release_manifest(root, publisher, 2, "build-001", "replacement archive")

    assert {:error, :immutable_build_changed} = Publisher.publish(root, publisher, changed)
  end

  test "will not sign metadata for another publisher", %{root: root} do
    signing_identity = Identity.generate()
    different_publisher = Identity.generate()
    unsigned = release_manifest(root, different_publisher, 1, "build-001", "archive one")

    assert {:error, :publisher_mismatch} = Publisher.publish(root, signing_identity, unsigned)
  end

  test "refuses a symlink in the immutable blob store", %{root: root, publisher: publisher} do
    unsigned = release_manifest(root, publisher, 1, "build-001", "archive one")
    [release] = unsigned["releases"]
    blob = Path.join([root, "blobs", release["sha256"] <> ".tar.gz"])
    target = Path.join(root, "archive")
    File.write!(target, "archive one")
    File.rm!(blob)
    File.ln_s!(target, blob)

    assert {:error, :unsafe_archive} = Publisher.publish(root, publisher, unsigned)
  end

  test "fails closed while another publisher holds the root lock", %{
    root: root,
    publisher: publisher
  } do
    unsigned = release_manifest(root, publisher, 1, "build-001", "archive one")
    lock = Path.join(root, ".publish.lock")
    File.write!(lock, "held")

    assert {:error, :eexist} = Publisher.publish(root, publisher, unsigned)
    refute File.exists?(Path.join([root, "channels", "stable.json"]))
  end

  test "renews expiry and rejects removal of a previously published build", ctx do
    first = release_manifest(ctx.root, ctx.publisher, 1, "build-001", "archive one")
    assert :ok = Publisher.publish(ctx.root, ctx.publisher, first)
    renewed = first |> Map.put("sequence", 2) |> Map.update!("expires_at", &(&1 + 100))
    assert :ok = Publisher.publish(ctx.root, ctx.publisher, renewed)
    replacement = release_manifest(ctx.root, ctx.publisher, 3, "build-002", "archive two")

    assert {:error, :immutable_build_changed} =
             Publisher.publish(ctx.root, ctx.publisher, replacement)
  end

  test "rejects expired publication without exposing a channel", ctx do
    expired =
      release_manifest(ctx.root, ctx.publisher, 1, "build-001", "archive one")
      |> Map.put("expires_at", 1)

    assert {:error, :expired} = Publisher.publish(ctx.root, ctx.publisher, expired)
    refute File.exists?(Path.join([ctx.root, "channels", "stable.json"]))
  end

  defp release_manifest(root, publisher, sequence, build, bytes) do
    digest = :crypto.hash(:sha256, bytes) |> Base.encode16(case: :lower)
    File.write!(Path.join([root, "blobs", digest <> ".tar.gz"]), bytes)

    %{
      "schema_version" => 1,
      "channel" => "stable",
      "publisher" => Identity.encode_public_key(publisher),
      "sequence" => sequence,
      "expires_at" => System.system_time(:second) + 3_600,
      "releases" => [
        %{
          "version" => "0.4.#{sequence}",
          "build" => build,
          "runtime" => "otp-28",
          "platform" => %{"os" => "darwin", "arch" => "aarch64"},
          "size" => byte_size(bytes),
          "sha256" => digest,
          "sources" => [
            %{
              "build" => "build-previous",
              "runtime" => "otp-28",
              "upgrade_plan_sha256" => String.duplicate("b", 64),
              "downgrade_plan_sha256" => String.duplicate("b", 64)
            }
          ],
          "restart_required" => false,
          "withdrawn" => false,
          "eligible" => true
        }
      ]
    }
  end
end
