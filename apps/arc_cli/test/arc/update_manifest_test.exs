defmodule Arc.CLI.Update.ManifestTest do
  use ExUnit.Case, async: true

  alias Arc.CLI.Update.Manifest
  alias Arc.Identity

  @now 1_789_000_000
  @digest String.duplicate("a", 64)
  @plan String.duplicate("b", 64)

  test "signs and verifies a pinned stable channel" do
    publisher = Identity.generate()
    unsigned = manifest(publisher)

    assert {:ok, signed} = Manifest.sign(publisher, unsigned)

    assert {:ok, verified} =
             Manifest.verify(signed,
               expected_publisher: Identity.encode_public_key(publisher),
               expected_channel: "stable",
               last_sequence: nil,
               last_digest: nil,
               now: @now
             )

    assert verified.manifest == unsigned
    assert byte_size(verified.digest) == 64
  end

  test "rejects unknown schema keys before signature verification" do
    publisher = Identity.generate()
    assert {:ok, signed} = Manifest.sign(publisher, manifest(publisher))

    injected = Map.put(signed, "surprise", true)

    assert {:error, :invalid_manifest} =
             verify(injected, publisher, last_sequence: nil, last_digest: nil)
  end

  test "rejects expired, replayed, and mismatched equal-sequence metadata" do
    publisher = Identity.generate()
    assert {:ok, signed} = Manifest.sign(publisher, manifest(publisher))
    assert {:ok, verified} = verify(signed, publisher, last_sequence: nil, last_digest: nil)

    assert {:ok, ^verified} =
             verify(signed, publisher,
               last_sequence: verified.manifest["sequence"],
               last_digest: verified.digest
             )

    assert {:error, :sequence_digest_mismatch} =
             verify(signed, publisher,
               last_sequence: verified.manifest["sequence"],
               last_digest: String.duplicate("0", 64)
             )

    assert {:error, :replayed_sequence} =
             verify(signed, publisher,
               last_sequence: verified.manifest["sequence"] + 1,
               last_digest: verified.digest
             )

    expired = manifest(publisher, %{"expires_at" => @now})
    assert {:ok, expired_signed} = Manifest.sign(publisher, expired)

    assert {:error, :expired} =
             verify(expired_signed, publisher, last_sequence: nil, last_digest: nil)
  end

  test "rejects a release with an ambiguous build and platform" do
    publisher = Identity.generate()
    release = release("0.3.3", "build-003")
    unsigned = manifest(publisher, %{"releases" => [release, release]})

    assert {:error, :duplicate_release} = Manifest.sign(publisher, unsigned)
  end

  test "select reports latest separately and only returns newer exact hot edges" do
    publisher = Identity.generate()

    restart =
      release("0.4.0", "build-004", %{
        "restart_required" => true,
        "sources" => [source("build-003", "otp-28")]
      })

    hot = release("0.3.3", "build-003", %{"sources" => [source("build-002", "otp-28")]})

    assert {:ok, signed} =
             Manifest.sign(publisher, manifest(publisher, %{"releases" => [hot, restart]}))

    assert {:ok, verified} = verify(signed, publisher, last_sequence: nil, last_digest: nil)

    assert {:ok, selection} =
             Manifest.select(verified,
               installed_build: "build-002",
               installed_version: "0.3.2",
               installed_runtime: "otp-28",
               platform: %{"os" => "darwin", "arch" => "aarch64"}
             )

    assert selection.latest == restart
    assert selection.status == :available
    assert selection.reason == :compatible_hot_edge
    assert selection.eligible.release == hot
    assert selection.eligible.source == source("build-002", "otp-28")
  end

  test "selection blocks a newer target that changes the runtime" do
    publisher = Identity.generate()

    runtime_change =
      release("0.3.3", "build-003", %{
        "runtime" => "otp-29",
        "sources" => [source("build-002", "otp-28")]
      })

    assert {:ok, signed} =
             Manifest.sign(publisher, manifest(publisher, %{"releases" => [runtime_change]}))

    assert {:ok, verified} = verify(signed, publisher, last_sequence: nil, last_digest: nil)
    assert {:ok, selection} = select(verified, pin: nil)

    assert selection.latest == runtime_change
    assert selection.status == :blocked
    assert selection.reason == :no_compatible_hot_edge
    assert is_nil(selection.eligible)
  end

  test "selection will not silently choose a lower SemVer target or advance a pin" do
    publisher = Identity.generate()
    lower = release("0.3.1", "build-001", %{"sources" => [source("build-002", "otp-28")]})
    assert {:ok, signed} = Manifest.sign(publisher, manifest(publisher, %{"releases" => [lower]}))
    assert {:ok, verified} = verify(signed, publisher, last_sequence: nil, last_digest: nil)

    assert {:ok, lower_selection} =
             select(verified, pin: nil)

    assert lower_selection.status == :blocked
    assert lower_selection.reason == :lower_semver_target
    assert is_nil(lower_selection.eligible)

    newer = release("0.3.3", "build-003", %{"sources" => [source("build-002", "otp-28")]})

    assert {:ok, newer_signed} =
             Manifest.sign(publisher, manifest(publisher, %{"releases" => [newer]}))

    assert {:ok, newer_verified} =
             verify(newer_signed, publisher, last_sequence: nil, last_digest: nil)

    assert {:ok, pinned_selection} = select(newer_verified, pin: "build-002")
    assert pinned_selection.status == :pinned
    assert pinned_selection.reason == :pinned
    assert is_nil(pinned_selection.eligible)
    assert pinned_selection.latest == newer
  end

  defp verify(document, publisher, options) do
    Manifest.verify(
      document,
      Keyword.merge(
        [
          expected_publisher: Identity.encode_public_key(publisher),
          expected_channel: "stable",
          now: @now
        ],
        options
      )
    )
  end

  defp select(verified, extra) do
    Manifest.select(
      verified,
      Keyword.merge(
        [
          installed_build: "build-002",
          installed_version: "0.3.2",
          installed_runtime: "otp-28",
          platform: %{"os" => "darwin", "arch" => "aarch64"}
        ],
        extra
      )
    )
  end

  defp manifest(publisher, overrides \\ %{}) do
    Map.merge(
      %{
        "schema_version" => 1,
        "channel" => "stable",
        "publisher" => Identity.encode_public_key(publisher),
        "sequence" => 7,
        "expires_at" => @now + 3_600,
        "releases" => [release("0.3.3", "build-003")]
      },
      overrides
    )
  end

  defp release(version, build, overrides \\ %{}) do
    Map.merge(
      %{
        "version" => version,
        "build" => build,
        "runtime" => "otp-28",
        "platform" => %{"os" => "darwin", "arch" => "aarch64"},
        "size" => 123,
        "sha256" => @digest,
        "sources" => [source("build-002", "otp-28")],
        "restart_required" => false,
        "withdrawn" => false,
        "eligible" => true
      },
      overrides
    )
  end

  defp source(build, runtime) do
    %{
      "build" => build,
      "runtime" => runtime,
      "upgrade_plan_sha256" => @plan,
      "downgrade_plan_sha256" => @plan
    }
  end
end
