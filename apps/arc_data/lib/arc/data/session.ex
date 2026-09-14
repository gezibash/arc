defmodule Arc.Data.Session do
  @moduledoc """
  Encrypted session between two agents.

  Session establishment:
    1. Both agents publish X25519 public keys via control plane
    2. Each derives shared secret: ECDH(my_x25519_priv, their_x25519_pub)
    3. Session key = HKDF-SHA256(shared_secret, info: "arc-session-v1")
    4. All messages encrypted with ChaCha20-Poly1305 using session key

  Each session has a unique session_id and a monotonic sequence counter
  used in the packet header for replay prevention.
  """

  alias Arc.Identity
  alias Arc.Identity.HKDF

  @hkdf_info "arc-session-v1"
  @nonce_bytes 12

  @type t :: %__MODULE__{
          peer_public_key: Identity.public_key(),
          session_key: binary(),
          session_id: binary(),
          seq: non_neg_integer(),
          my_identity: Identity.t()
        }

  @enforce_keys [:peer_public_key, :session_key, :session_id, :seq, :my_identity]
  defstruct [:peer_public_key, :session_key, :session_id, :seq, :my_identity]

  @doc """
  Establish a session with a peer given their Ed25519 public key and X25519 public key.
  Derives a shared secret via ECDH and a session key via HKDF-SHA256.
  """
  @spec establish(Identity.t(), Identity.public_key(), binary()) :: t()
  def establish(
        %Identity{} = my_identity,
        <<peer_ed_pub::binary-size(32)>>,
        <<peer_x25519_pub::binary-size(32)>>
      ) do
    {_my_x_pub, my_x_priv} = Identity.to_x25519(my_identity)
    shared_secret = :crypto.compute_key(:ecdh, peer_x25519_pub, my_x_priv, :x25519)

    key = HKDF.derive(shared_secret, <<0::256>>, @hkdf_info, 32)

    %__MODULE__{
      peer_public_key: peer_ed_pub,
      session_key: key,
      session_id: :crypto.strong_rand_bytes(16),
      seq: 0,
      my_identity: my_identity
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
