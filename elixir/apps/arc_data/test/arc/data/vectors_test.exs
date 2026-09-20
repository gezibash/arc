defmodule Arc.Data.VectorsTest do
  # The file test/vectors/identity.json holds the values that every ARC
  # implementation must produce. The Go suite reads the same file. A change to
  # a vector is a change to the protocol.
  #
  # To write the file again: mix run --no-start scripts/write-vectors.exs
  use ExUnit.Case, async: true

  alias Arc.Data.Packet
  alias Arc.Data.Session
  alias Arc.Identity
  alias Arc.Identity.HKDF
  alias Arc.Identity.SealedBox

  @path Path.expand("../../../../../test/vectors/identity.json", __DIR__)
  @external_resource @path

  setup_all do
    case File.read(@path) do
      {:ok, body} ->
        %{vectors: :json.decode(body)}

      {:error, reason} ->
        flunk("""
        #{@path}: #{:file.format_error(reason)}
        Run: mix run --no-start scripts/write-vectors.exs
        """)
    end
  end

  defp hex(binary), do: Base.encode16(binary, case: :lower)
  defp unhex(string), do: Base.decode16!(string, case: :lower)
  defp identity(seed_hex), do: seed_hex |> unhex() |> Identity.from_seed()

  test "every identity matches", %{vectors: vectors} do
    for want <- vectors["identities"] do
      identity = identity(want["seed"])
      {x_public, x_secret} = Identity.to_x25519(identity)

      assert Identity.encode_public_key(identity) == want["public_key"]
      assert hex(x_public) == want["x25519_public"]
      assert hex(x_secret) == want["x25519_secret"]
      assert Identity.name(identity) == want["name"]
      assert Identity.short_name(identity) == want["short_name"]

      signature = unhex(want["signature_of_arc"])
      assert Identity.verify(identity.public_key, "arc", signature)
    end
  end

  test "the public key gives the same X25519 key as the secret side", %{vectors: vectors} do
    for want <- vectors["identities"] do
      {:ok, derived} = Identity.public_key_to_x25519(unhex(want["public_key"]))
      assert hex(derived) == want["x25519_public"]
    end
  end

  test "every key derivation matches", %{vectors: vectors} do
    for want <- vectors["hkdf"] do
      key = HKDF.derive(unhex(want["ikm"]), unhex(want["salt"]), want["info"], want["length"])
      assert hex(key) == want["output"]
    end
  end

  test "the sealed box opens", %{vectors: vectors} do
    want = vectors["sealed_box"]
    recipient = identity(want["recipient_seed"])

    assert {:ok, plaintext} = SealedBox.open(recipient, unhex(want["sealed"]))
    assert plaintext == want["plaintext"]
  end

  test "the session key follows from the ephemeral key", %{vectors: vectors} do
    want = vectors["session"]
    initiator = identity(want["initiator_seed"])
    responder = identity(want["responder_seed"])

    session =
      Session.accept(
        responder,
        initiator.public_key,
        unhex(want["ephemeral_public"]),
        unhex(want["session_id"])
      )

    assert hex(session.session_key) == want["session_key"]
    assert session.version == want["version"]
  end

  test "the packet decodes, and the message inside it reads", %{vectors: vectors} do
    want = vectors["session"]
    initiator = identity(want["initiator_seed"])
    responder = identity(want["responder_seed"])

    assert {:ok, packet} = Packet.decode(unhex(want["packet"]))
    assert packet.src == initiator.public_key
    assert packet.dst == responder.public_key
    assert hex(packet.session_id) == want["session_id"]
    assert hex(packet.ek) == want["ephemeral_public"]
    assert packet.seq == want["seq"]
    assert packet.ts == want["ts"]

    session = Session.accept(responder, packet.src, packet.ek, packet.session_id)
    assert {:ok, plaintext} = Session.decrypt(session, packet.nonce, packet.ciphertext)
    assert plaintext == want["plaintext"]
  end
end
