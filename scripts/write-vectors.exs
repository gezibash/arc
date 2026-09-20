# Writes the shared protocol vectors that every ARC implementation must match.
#
#     mix run --no-start scripts/write-vectors.exs
#
# A change to a vector is a change to the protocol. See docs/go/PLAN.md.

alias Arc.Data.CapabilityPackage
alias Arc.Data.Packet
alias Arc.Data.RelayAnnouncement
alias Arc.Data.Session
alias Arc.Identity
alias Arc.Identity.HKDF
alias Arc.Identity.SealedBox

seeds = [
  "421151a459faeade3d247115f94aedae42318124095afabe4d1451a559faedee",
  "0000000000000000000000000000000000000000000000000000000000000000",
  "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
  "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
]

identities =
  Enum.map(seeds, fn hex ->
    identity = hex |> Base.decode16!(case: :lower) |> Identity.from_seed()
    {x_public, x_secret} = Identity.to_x25519(identity)

    %{
      "seed" => hex,
      "public_key" => Identity.encode_public_key(identity),
      "x25519_public" => Base.encode16(x_public, case: :lower),
      "x25519_secret" => Base.encode16(x_secret, case: :lower),
      "name" => Identity.name(identity),
      "short_name" => Identity.short_name(identity),
      "signature_of_arc" => identity |> Identity.sign("arc") |> Base.encode16(case: :lower)
    }
  end)

hkdf =
  for {ikm, salt, info, len} <- [
        {"", "", "arc-sealed-v1", 32},
        {"ikm", "salt", "arc-session-v2", 32},
        {"0123456789abcdef", "", "arc-session-v1", 64}
      ] do
    %{
      "ikm" => Base.encode16(ikm, case: :lower),
      "salt" => Base.encode16(salt, case: :lower),
      "info" => info,
      "length" => len,
      "output" => ikm |> HKDF.derive(salt, info, len) |> Base.encode16(case: :lower)
    }
  end

# A sealed box carries a random ephemeral key, so the bytes differ on every
# call. The vector therefore holds a sealed box and the text inside it.
recipient = Identity.from_seed(Base.decode16!(hd(seeds), case: :lower))
plaintext = "the seal opens for one key only"
sealed = SealedBox.seal(recipient |> Identity.to_x25519() |> elem(0), plaintext)

# A session carries a random ephemeral key, a random session id and a random
# nonce. The vector therefore records the values of one real session, and both
# implementations replay that session from the packet.
initiator = Identity.from_seed(Base.decode16!(Enum.at(seeds, 3), case: :lower))
responder = recipient
{:ok, session} = Session.establish(initiator, responder.public_key)
message = "one packet, two implementations"
{nonce, ciphertext, seq, _session} = Session.encrypt(session, message)
timestamp = 1_735_689_600_000
timestamp_seconds = div(timestamp, 1000)

packet =
  Packet.encode(initiator, responder.public_key, session.session_id, seq, nonce, ciphertext,
    ts: timestamp,
    ek: session.ek_pub
  )

# An announcement signs the canonical JSON of its own record. A different
# canonical form gives a different signature, so this record pins the encoder
# of every implementation.
announcement =
  RelayAnnouncement.create(
    initiator,
    [
      %{
        "id" => "exec",
        "kind" => "tool",
        "scheme" => "exec",
        "title" => "Run a command",
        "summary" => "Runs one command, and returns the output.",
        "invocation_mode" => "request",
        "release_version" => "0.6.0",
        "channel" => "stable",
        "detail_path" => "/capabilities/exec"
      },
      %{"id" => "dm"}
    ],
    now: timestamp_seconds,
    ttl: 180
  )

# A capability package is signed after it is normalized. A different
# normalizer, or a different canonical form, gives a different hash. These
# records therefore pin both.
packages =
  for path <- [
        "providers/exec/manifest.json",
        "test/fixtures/providers/sqlite-provider.json",
        "test/fixtures/providers/users-provider.json"
      ] do
    {:ok, package} = CapabilityPackage.load_file(Path.join(File.cwd!(), path))
    signed = CapabilityPackage.sign(initiator, package)

    %{
      "path" => path,
      "signer_seed" => Enum.at(seeds, 3),
      "package_hash" => signed["package_hash"],
      "signature" => get_in(signed, ["signature", "value"]),
      "signed" => signed
    }
  end

# A release channel is signed by its publisher. Every implementation must
# reach the same bytes, or a citizen refuses an update that is good.
channel_unsigned = %{
  "schema_version" => 2,
  "channel" => "stable",
  "publisher" => Arc.Identity.encode_public_key(initiator),
  "sequence" => 7,
  "expires_at" => timestamp_seconds + 86_400,
  "releases" => [
    %{
      "version" => "0.7.0",
      "build" => "0.7.0+abc1234",
      "runtime" => "go1.27.1",
      "platform" => %{"os" => "darwin", "arch" => "arm64"},
      "size" => 12_345_678,
      "sha256" => String.duplicate("ab", 32),
      "sources" => [],
      "restart_required" => true,
      "withdrawn" => false,
      "eligible" => true,
      "install" => %{"size" => 12_345_678, "sha256" => String.duplicate("ab", 32)}
    }
  ]
}

{:ok, channel_signed} = Arc.CLI.Update.Manifest.sign(initiator, channel_unsigned)

vectors = %{
  "version" => 1,
  "note" => "Generated by scripts/write-vectors.exs. A change here changes the protocol.",
  "identities" => identities,
  "hkdf" => hkdf,
  "sealed_box" => %{
    "recipient_seed" => hd(seeds),
    "plaintext" => plaintext,
    "sealed" => Base.encode16(sealed, case: :lower)
  },
  "announcement" => %{
    "signer_seed" => Enum.at(seeds, 3),
    "now" => timestamp_seconds,
    "record" => announcement
  },
  "packages" => packages,
  "release_channel" => %{
    "publisher_seed" => Enum.at(seeds, 3),
    "now" => timestamp_seconds,
    "document" => channel_signed
  },
  "session" => %{
    "version" => 2,
    "initiator_seed" => Enum.at(seeds, 3),
    "responder_seed" => hd(seeds),
    "ephemeral_public" => Base.encode16(session.ek_pub, case: :lower),
    "session_id" => Base.encode16(session.session_id, case: :lower),
    "session_key" => Base.encode16(session.session_key, case: :lower),
    "seq" => seq,
    "ts" => timestamp,
    "plaintext" => message,
    "packet" => Base.encode16(packet, case: :lower)
  }
}

path = Path.join([File.cwd!(), "test", "vectors", "identity.json"])
File.mkdir_p!(Path.dirname(path))
File.write!(path, [:json.encode(vectors), "\n"])
IO.puts("wrote #{path}")
