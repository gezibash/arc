defmodule Arc.Identity.HKDF do
  @moduledoc """
  HKDF-SHA256 (RFC 5869): extract, then expand.

  One implementation for every key derivation in ARC. `Arc.Data.Session`
  and `Arc.Identity.SealedBox` both call `derive/4`.
  """

  @hash :sha256
  @hash_len 32

  @doc """
  Derive `len` bytes of key material from `ikm`.

  `salt` must be a binary. An empty salt is replaced by `hash_len` zero
  bytes, as the RFC requires. `len` must be at most 255 * 32 bytes.
  """
  @spec derive(binary(), binary(), binary(), pos_integer()) :: binary()
  def derive(ikm, salt, info, len)
      when is_binary(ikm) and is_binary(salt) and is_binary(info) and is_integer(len) and
             len > 0 and len <= 255 * @hash_len do
    salt = if salt == "", do: <<0::size(@hash_len * 8)>>, else: salt
    prk = :crypto.mac(:hmac, @hash, salt, ikm)
    expand(prk, info, len, 1, "", "")
  end

  defp expand(_prk, _info, len, _n, _prev, acc) when byte_size(acc) >= len do
    binary_part(acc, 0, len)
  end

  defp expand(prk, info, len, n, prev, acc) do
    block = :crypto.mac(:hmac, @hash, prk, prev <> info <> <<n>>)
    expand(prk, info, len, n + 1, block, acc <> block)
  end
end
