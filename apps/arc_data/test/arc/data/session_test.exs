defmodule Arc.Data.SessionTest do
  use ExUnit.Case, async: true

  alias Arc.Data.Session
  alias Arc.Identity

  describe "establish/3" do
    test "the responder accepts the initiator's ephemeral key and derives the same session key" do
      alice = Identity.generate()
      bob = Identity.generate()

      {:ok, session_a} = Session.establish(alice, bob.public_key)
      session_b = Session.accept(bob, alice.public_key, session_a.ek_pub, session_a.session_id)

      assert session_a.session_key == session_b.session_key
      assert session_b.session_id == session_a.session_id
      assert session_a.version == 2 and session_b.version == 2
      assert byte_size(session_a.ek_pub) == 32
    end

    test "two sessions between the same peers have different keys" do
      alice = Identity.generate()
      bob = Identity.generate()

      {:ok, s1} = Session.establish(alice, bob.public_key)
      {:ok, s2} = Session.establish(alice, bob.public_key)

      refute s1.session_key == s2.session_key
      refute s1.ek_pub == s2.ek_pub
    end

    test "a wrong ephemeral key derives a different key" do
      alice = Identity.generate()
      bob = Identity.generate()
      {other_ek, _} = :crypto.generate_key(:ecdh, :x25519)

      {:ok, session_a} = Session.establish(alice, bob.public_key)
      session_b = Session.accept(bob, alice.public_key, other_ek, session_a.session_id)

      refute session_a.session_key == session_b.session_key
    end

    test "v1 sessions still derive one shared key from both static keys" do
      alice = Identity.generate()
      bob = Identity.generate()

      {:ok, session_a} = Session.establish_v1(alice, bob.public_key)
      {:ok, session_b} = Session.establish_v1(bob, alice.public_key)

      assert session_a.session_key == session_b.session_key
      assert session_a.version == 1
      assert is_nil(session_a.ek_pub)
    end

    test "session key is 32 bytes" do
      alice = Identity.generate()
      bob = Identity.generate()

      {:ok, session} = Session.establish(alice, bob.public_key)
      assert byte_size(session.session_key) == 32
    end

    test "different keypairs produce different session keys" do
      alice = Identity.generate()
      bob = Identity.generate()
      carol = Identity.generate()

      {:ok, session_ab} = Session.establish(alice, bob.public_key)
      {:ok, session_ac} = Session.establish(alice, carol.public_key)

      refute session_ab.session_key == session_ac.session_key
    end

    test "generates a session_id" do
      alice = Identity.generate()
      bob = Identity.generate()

      {:ok, session} = Session.establish(alice, bob.public_key)
      assert byte_size(session.session_id) == 16
    end

    test "each session gets a unique session_id" do
      alice = Identity.generate()
      bob = Identity.generate()

      {:ok, s1} = Session.establish(alice, bob.public_key)
      {:ok, s2} = Session.establish(alice, bob.public_key)
      refute s1.session_id == s2.session_id
    end

    test "seq starts at 0" do
      alice = Identity.generate()
      bob = Identity.generate()

      {:ok, session} = Session.establish(alice, bob.public_key)
      assert session.seq == 0
    end
  end

  describe "encrypt/2 and decrypt/3" do
    setup do
      alice = Identity.generate()
      bob = Identity.generate()

      {:ok, session_a} = Session.establish(alice, bob.public_key)
      session_b = Session.accept(bob, alice.public_key, session_a.ek_pub, session_a.session_id)

      %{session_a: session_a, session_b: session_b}
    end

    test "encrypt then decrypt roundtrip", %{session_a: sa, session_b: sb} do
      msg = "hello from alice"
      {nonce, ciphertext, _seq, _sa2} = Session.encrypt(sa, msg)
      assert {:ok, ^msg} = Session.decrypt(sb, nonce, ciphertext)
    end

    test "encrypt increments seq", %{session_a: sa} do
      assert sa.seq == 0
      {_, _, seq1, sa2} = Session.encrypt(sa, "first")
      assert seq1 == 0
      assert sa2.seq == 1
      {_, _, seq2, sa3} = Session.encrypt(sa2, "second")
      assert seq2 == 1
      assert sa3.seq == 2
    end

    test "decrypt with wrong key fails" do
      alice = Identity.generate()
      bob = Identity.generate()
      carol = Identity.generate()

      {:ok, session_ab} = Session.establish(alice, bob.public_key)
      {:ok, session_ac} = Session.establish(alice, carol.public_key)

      {nonce, ciphertext, _seq, _} = Session.encrypt(session_ab, "secret")
      assert {:error, :decrypt_failed} = Session.decrypt(session_ac, nonce, ciphertext)
    end

    test "ciphertext is different from plaintext", %{session_a: sa} do
      msg = "hello"
      {_nonce, ciphertext, _, _} = Session.encrypt(sa, msg)
      refute ciphertext == msg
    end

    test "each encryption produces unique nonce", %{session_a: sa} do
      {nonce1, _, _, sa2} = Session.encrypt(sa, "hello")
      {nonce2, _, _, _} = Session.encrypt(sa2, "hello")
      refute nonce1 == nonce2
    end
  end
end
