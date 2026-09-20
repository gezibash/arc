defmodule Arc.Identity.SealedBox do
  @moduledoc """
  Seal a message to a recipient's X25519 public key. No session is needed.
  Only the holder of the matching identity can open it.

  Format, version 1:

      <<1, ephemeral_pub::32, ciphertext_and_tag::binary>>

  Construction:

    1. Generate an ephemeral X25519 keypair.
    2. shared = ECDH(ephemeral_priv, recipient_pub)
    3. key = HKDF-SHA256(shared, salt: ephemeral_pub <> recipient_pub,
       info: "arc-sealed-v1", len: 32)
    4. ChaCha20-Poly1305 with a zero nonce and the ephemeral public key as
       associated data. The key is single use, so a fixed nonce is safe.

  `open/2` never reports why it failed. A wrong key and a corrupt body both
  return `{:error, :open_failed}`.
  """

  alias Arc.Identity
  alias Arc.Identity.HKDF

  @version 1
  @info "arc-sealed-v1"
  @nonce <<0::96>>
  @tag_bytes 16

  @doc """
  Seal `plaintext` to the recipient's X25519 public key.
  """
  @spec seal(<<_::256>>, binary()) :: binary()
  def seal(<<recipient_pub::binary-size(32)>>, plaintext) when is_binary(plaintext) do
    {ek_pub, ek_priv} = :crypto.generate_key(:ecdh, :x25519)
    shared = :crypto.compute_key(:ecdh, recipient_pub, ek_priv, :x25519)
    key = derive_key(shared, ek_pub, recipient_pub)

    {ciphertext, tag} =
      :crypto.crypto_one_time_aead(:chacha20_poly1305, key, @nonce, plaintext, ek_pub, true)

    <<@version, ek_pub::binary, ciphertext::binary, tag::binary>>
  end

  @doc """
  Open a sealed message with the recipient's identity.
  """
  @spec open(Identity.t(), binary()) :: {:ok, binary()} | {:error, :open_failed}
  def open(%Identity{} = identity, <<@version, ek_pub::binary-size(32), rest::binary>>)
      when byte_size(rest) >= @tag_bytes do
    {my_pub, my_priv} = Identity.to_x25519(identity)
    shared = :crypto.compute_key(:ecdh, ek_pub, my_priv, :x25519)
    key = derive_key(shared, ek_pub, my_pub)
    ct_size = byte_size(rest) - @tag_bytes
    <<ciphertext::binary-size(ct_size), tag::binary-size(@tag_bytes)>> = rest

    case :crypto.crypto_one_time_aead(
           :chacha20_poly1305,
           key,
           @nonce,
           ciphertext,
           ek_pub,
           tag,
           false
         ) do
      plaintext when is_binary(plaintext) -> {:ok, plaintext}
      :error -> {:error, :open_failed}
    end
  end

  def open(%Identity{}, sealed) when is_binary(sealed), do: {:error, :open_failed}

  defp derive_key(shared, ek_pub, recipient_pub) do
    HKDF.derive(shared, ek_pub <> recipient_pub, @info, 32)
  end
end
