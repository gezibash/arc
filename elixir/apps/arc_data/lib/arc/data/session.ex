defmodule Arc.Data.Session do
  @moduledoc """
  Encrypted session between two agents.

  Session establishment, version 2:
    1. The initiator computes the responder's X25519 public key from the
       responder's Ed25519 public key. See `Arc.Identity.public_key_to_x25519/1`.
    2. The initiator generates an ephemeral X25519 keypair per session and
       derives shared = ECDH(ephemeral_priv, responder_x25519_pub).
    3. Session key = HKDF-SHA256(shared, salt: ephemeral_pub, info: "arc-session-v2").
    4. Every packet header carries the ephemeral public key. The responder
       derives the same key with ECDH(my_x25519_priv, ephemeral_pub).
    5. All messages are encrypted with ChaCha20-Poly1305 under the session key.

  Compromise of the initiator's long-term key does not expose past
  sessions. Compromise of the responder's does. A responder ephemeral is
  version 3 work.

  Version 1 derived the key from both long-term keys and carried no
  ephemeral. `establish_v1/3` remains so a peer on the previous release can
  still be decrypted. It is deprecated.

  Each session has a unique session_id and a monotonic sequence counter
  used in the packet header for replay prevention.
  """

  alias Arc.Identity
  alias Arc.Identity.HKDF

  @hkdf_info_v1 "arc-session-v1"
  @hkdf_info_v2 "arc-session-v2"
  @nonce_bytes 12

  @type t :: %__MODULE__{
          peer_public_key: Identity.public_key(),
          session_key: binary(),
          session_id: binary(),
          seq: non_neg_integer(),
          my_identity: Identity.t(),
          ek_pub: binary() | nil,
          version: 1 | 2
        }

  @enforce_keys [:peer_public_key, :session_key, :session_id, :seq, :my_identity, :version]
  defstruct [:peer_public_key, :session_key, :session_id, :seq, :my_identity, :ek_pub, :version]

  @doc """
  Start a version 2 session as the initiator from the peer's Ed25519 public
  key. Generates an ephemeral key that every packet of this session carries
  in its header.
  """
  @spec establish(Identity.t(), Identity.public_key()) ::
          {:ok, t()} | {:error, :invalid_public_key}
  def establish(%Identity{} = my_identity, <<peer_ed_pub::binary-size(32)>>) do
    with {:ok, peer_x25519_pub} <- Identity.public_key_to_x25519(peer_ed_pub) do
      {:ok, establish_v2(my_identity, peer_ed_pub, peer_x25519_pub)}
    end
  end

  defp establish_v2(my_identity, peer_ed_pub, peer_x25519_pub) do
    {ek_pub, ek_priv} = :crypto.generate_key(:ecdh, :x25519)
    shared_secret = :crypto.compute_key(:ecdh, peer_x25519_pub, ek_priv, :x25519)

    %__MODULE__{
      peer_public_key: peer_ed_pub,
      session_key: HKDF.derive(shared_secret, ek_pub, @hkdf_info_v2, 32),
      session_id: :crypto.strong_rand_bytes(16),
      seq: 0,
      my_identity: my_identity,
      ek_pub: ek_pub,
      version: 2
    }
  end

  @doc """
  Join a version 2 session as the responder, from the ephemeral public key
  and session id in the initiator's packet header.
  """
  @spec accept(Identity.t(), Identity.public_key(), binary(), binary()) :: t()
  def accept(
        %Identity{} = my_identity,
        <<peer_ed_pub::binary-size(32)>>,
        <<ek_pub::binary-size(32)>>,
        <<session_id::binary-size(16)>>
      ) do
    {_my_x_pub, my_x_priv} = Identity.to_x25519(my_identity)
    shared_secret = :crypto.compute_key(:ecdh, ek_pub, my_x_priv, :x25519)

    %__MODULE__{
      peer_public_key: peer_ed_pub,
      session_key: HKDF.derive(shared_secret, ek_pub, @hkdf_info_v2, 32),
      session_id: session_id,
      seq: 0,
      my_identity: my_identity,
      ek_pub: ek_pub,
      version: 2
    }
  end

  @doc """
  Version 1 session from both long-term keys. Deprecated. Kept so packets
  from a peer on the previous release still decrypt.
  """
  @spec establish_v1(Identity.t(), Identity.public_key()) ::
          {:ok, t()} | {:error, :invalid_public_key}
  def establish_v1(%Identity{} = my_identity, <<peer_ed_pub::binary-size(32)>>) do
    with {:ok, peer_x25519_pub} <- Identity.public_key_to_x25519(peer_ed_pub) do
      {:ok, v1_session(my_identity, peer_ed_pub, peer_x25519_pub)}
    end
  end

  defp v1_session(my_identity, peer_ed_pub, peer_x25519_pub) do
    {_my_x_pub, my_x_priv} = Identity.to_x25519(my_identity)
    shared_secret = :crypto.compute_key(:ecdh, peer_x25519_pub, my_x_priv, :x25519)

    %__MODULE__{
      peer_public_key: peer_ed_pub,
      session_key: HKDF.derive(shared_secret, <<0::256>>, @hkdf_info_v1, 32),
      session_id: :crypto.strong_rand_bytes(16),
      seq: 0,
      my_identity: my_identity,
      ek_pub: nil,
      version: 1
    }
  end

  @doc """
  Encrypt a message. Returns {nonce, ciphertext, seq_used, updated_session}.

  The seq_used should be included in the packet header. The updated_session
  has an incremented sequence counter and must be stored by the caller.
  """
  @spec encrypt(t(), binary()) :: {binary(), binary(), non_neg_integer(), t()}
  def encrypt(%__MODULE__{session_key: key, seq: seq} = session, plaintext)
      when is_binary(plaintext) do
    nonce = :crypto.strong_rand_bytes(@nonce_bytes)

    {ciphertext, tag} =
      :crypto.crypto_one_time_aead(:chacha20_poly1305, key, nonce, plaintext, "", true)

    {nonce, ciphertext <> tag, seq, %{session | seq: seq + 1}}
  end

  @doc """
  Decrypt a message given nonce and ciphertext+tag.
  """
  @spec decrypt(t(), binary(), binary()) :: {:ok, binary()} | {:error, :decrypt_failed}
  def decrypt(%__MODULE__{session_key: key}, nonce, ciphertext_with_tag)
      when is_binary(nonce) and is_binary(ciphertext_with_tag) do
    tag_size = 16
    ct_size = byte_size(ciphertext_with_tag) - tag_size
    <<ciphertext::binary-size(ct_size), tag::binary-size(tag_size)>> = ciphertext_with_tag

    case :crypto.crypto_one_time_aead(:chacha20_poly1305, key, nonce, ciphertext, "", tag, false) do
      plaintext when is_binary(plaintext) -> {:ok, plaintext}
      :error -> {:error, :decrypt_failed}
    end
  end
end
