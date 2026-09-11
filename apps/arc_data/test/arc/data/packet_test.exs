defmodule Arc.Data.PacketTest do
  use ExUnit.Case, async: true

  import Bitwise

  alias Arc.Data.{Packet, Session}
  alias Arc.Identity

  setup do
    alice = Identity.generate()
    bob = Identity.generate()
    {bob_x_pub, _} = Identity.to_x25519(bob)
    session = Session.establish(alice, bob.public_key, bob_x_pub)
    %{alice: alice, bob: bob, session: session}
  end

  test "encode produces binary packet", %{alice: alice, bob: bob, session: session} do
    {nonce, ciphertext, seq, _} = Session.encrypt(session, "hello")
    packet = Packet.encode(alice, bob.public_key, session.session_id, seq, nonce, ciphertext)
    assert is_binary(packet)
    assert byte_size(packet) > 64
  end

  test "decode roundtrip", %{alice: alice, bob: bob, session: session} do
    {nonce, ciphertext, seq, _} = Session.encrypt(session, "hello")
    packet = Packet.encode(alice, bob.public_key, session.session_id, seq, nonce, ciphertext)

    assert {:ok, decoded} = Packet.decode(packet)
    assert decoded.src == alice.public_key
    assert decoded.dst == bob.public_key
    assert decoded.session_id == session.session_id
    assert decoded.seq == seq
    assert decoded.nonce == nonce
    assert decoded.ciphertext == ciphertext
    assert is_integer(decoded.ts)
  end

  test "decode verifies ed25519 signature", %{alice: alice, bob: bob, session: session} do
    {nonce, ciphertext, seq, _} = Session.encrypt(session, "hello")
    packet = Packet.encode(alice, bob.public_key, session.session_id, seq, nonce, ciphertext)

    # Flip a byte in the signature area (after header_len + header)
    <<header_len::32-big, rest::binary>> = packet
    <<header::binary-size(header_len), sig_byte, sig_rest::binary>> = rest
    flipped = bxor(sig_byte, 0xFF)
    corrupted = <<header_len::32-big>> <> header <> <<flipped>> <> sig_rest

    assert {:error, :invalid_signature} = Packet.decode(corrupted)
  end

  test "decode rejects tampered payload", %{alice: alice, bob: bob, session: session} do
    {nonce, ciphertext, seq, _} = Session.encrypt(session, "hello")
    packet = Packet.encode(alice, bob.public_key, session.session_id, seq, nonce, ciphertext)

    # Flip last byte of packet (in the payload)
    size = byte_size(packet)
    <<prefix::binary-size(size - 1), last_byte>> = packet
    flipped = bxor(last_byte, 0xFF)
    corrupted = prefix <> <<flipped>>

    assert {:error, :payload_hash_mismatch} = Packet.decode(corrupted)
  end

  test "decode rejects malformed binary" do
    assert {:error, :malformed_packet} = Packet.decode(<<0, 1, 2, 3>>)
    assert {:error, :malformed_packet} = Packet.decode(<<>>)
  end

  test "seq increments across messages are captured", %{alice: alice, bob: bob, session: session} do
    {n1, c1, seq1, session} = Session.encrypt(session, "first")
    {n2, c2, seq2, _session} = Session.encrypt(session, "second")

    p1 = Packet.encode(alice, bob.public_key, session.session_id, seq1, n1, c1)
    p2 = Packet.encode(alice, bob.public_key, session.session_id, seq2, n2, c2)

    {:ok, d1} = Packet.decode(p1)
    {:ok, d2} = Packet.decode(p2)

    assert d2.seq == d1.seq + 1
  end

  test "full encrypt-pack-decode-decrypt roundtrip", %{alice: alice, bob: bob} do
    {bob_x_pub, _} = Identity.to_x25519(bob)
    {alice_x_pub, _} = Identity.to_x25519(alice)

    session_a = Session.establish(alice, bob.public_key, bob_x_pub)
    session_b = Session.establish(bob, alice.public_key, alice_x_pub)

    plaintext = "the quick brown fox"
    {nonce, ciphertext, seq, _} = Session.encrypt(session_a, plaintext)
    packet = Packet.encode(alice, bob.public_key, session_a.session_id, seq, nonce, ciphertext)

    {:ok, decoded} = Packet.decode(packet)
    assert {:ok, ^plaintext} = Session.decrypt(session_b, decoded.nonce, decoded.ciphertext)
  end

  test "encode supports explicit timestamp override", %{alice: alice, bob: bob, session: session} do
    ts = 1_700_000_000_000
    {nonce, ciphertext, seq, _} = Session.encrypt(session, "hello")

    packet =
      Packet.encode(alice, bob.public_key, session.session_id, seq, nonce, ciphertext, ts: ts)

    assert {:ok, decoded} = Packet.decode(packet)
    assert decoded.ts == ts
  end
end
