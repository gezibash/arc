defmodule Arc.Data.Packet do
  @moduledoc """
  Arc wire packet format.

      [4 bytes]   header_length (big-endian uint32)
      [N bytes]   header (JSON): {src, dst, sid, seq, ts, ph}
      [64 bytes]  Ed25519 signature over header bytes
      [N bytes]   payload: nonce (12 bytes) <> ChaCha20-Poly1305 ciphertext

  Fields:
    src  base64(Ed25519 public key of sender)
    dst  base64(Ed25519 public key of recipient)
    sid  base64(session ID, 16 random bytes established at session setup)
    seq  monotonic sequence number per session (replay prevention)
    ts   unix milliseconds (freshness check)
    ph   base64(SHA-256(payload)) — binds payload to signed header

  The Ed25519 signature covers the raw header bytes, binding all header
  fields to the sender's keypair. A valid signature proves the sender
  holds the private key corresponding to src.

  ph binds the ciphertext to the signed header, preventing payload
  substitution attacks.

  Note: ph uses SHA-256 pending a Blake3 dependency (same security
  properties for this use case — wire compatibility will be maintained
  when upgraded).
  """

  alias Arc.Identity

  @signature_bytes 64
  @nonce_bytes 12

  @doc """
  Encode and sign a packet.
  """
  @spec encode(Identity.t(), binary(), binary(), non_neg_integer(), binary(), binary(), keyword()) ::
          binary()
  def encode(%Identity{} = src_identity, dst_pk, session_id, seq, nonce, ciphertext, opts \\ []) do
    payload = nonce <> ciphertext
    ts = Keyword.get(opts, :ts, System.system_time(:millisecond))

    header_map = %{
      "src" => Base.encode64(src_identity.public_key),
      "dst" => Base.encode64(dst_pk),
      "sid" => Base.encode64(session_id),
      "seq" => seq,
      "ts" => ts,
      "ph" => Base.encode64(:crypto.hash(:sha256, payload))
    }

    header_bytes = IO.iodata_to_binary(:json.encode(header_map))
    header_len = byte_size(header_bytes)
    signature = Identity.sign(src_identity, header_bytes)

    <<header_len::32-big>> <> header_bytes <> signature <> payload
  end

  @doc """
  Decode and verify a packet. Returns the decoded fields including
  separated nonce and ciphertext.

  Verifies:
    1. Ed25519 signature over header
    2. Payload hash matches ph in header
  """
  @spec decode(binary()) ::
          {:ok,
           %{
             src: binary(),
             dst: binary(),
             session_id: binary(),
             seq: non_neg_integer(),
             ts: non_neg_integer(),
             nonce: binary(),
             ciphertext: binary()
           }}
          | {:error, :malformed_packet | :invalid_signature | :payload_hash_mismatch}
  def decode(<<header_len::32-big, rest::binary>>) do
    case rest do
      <<header_bytes::binary-size(header_len), sig::binary-size(@signature_bytes),
        payload::binary>> ->
        verify_and_decode(header_bytes, sig, payload)

      _ ->
        {:error, :malformed_packet}
    end
  end

  def decode(_), do: {:error, :malformed_packet}

  defp verify_and_decode(header_bytes, sig, payload) do
    with {:ok, header} <- safe_decode_json(header_bytes),
         {:ok, src_pk} <- safe_decode64(header["src"]),
         {:sig, true} <- {:sig, Identity.verify(src_pk, header_bytes, sig)},
         expected_ph = Base.encode64(:crypto.hash(:sha256, payload)),
         {:ph, true} <- {:ph, expected_ph == header["ph"]},
         {:ok, dst_pk} <- safe_decode64(header["dst"]),
         {:ok, session_id} <- safe_decode64(header["sid"]),
         {:seq, true} <- {:seq, is_integer(header["seq"]) and header["seq"] >= 0},
         {:ts, true} <- {:ts, is_integer(header["ts"])},
         <<nonce::binary-size(@nonce_bytes), ciphertext::binary>> <- payload do
      {:ok,
       %{
         src: src_pk,
         dst: dst_pk,
         session_id: session_id,
         seq: header["seq"],
         ts: header["ts"],
         nonce: nonce,
         ciphertext: ciphertext
       }}
    else
      {:sig, false} -> {:error, :invalid_signature}
      {:ph, false} -> {:error, :payload_hash_mismatch}
      {:seq, false} -> {:error, :malformed_packet}
      {:ts, false} -> {:error, :malformed_packet}
      _ -> {:error, :malformed_packet}
    end
  end

  defp safe_decode_json(bytes) do
    try do
      {:ok, :json.decode(bytes)}
    rescue
      _ -> {:error, :malformed_packet}
    end
  end

  defp safe_decode64(nil), do: {:error, :malformed_packet}

  defp safe_decode64(str) do
    case Base.decode64(str) do
      {:ok, bytes} -> {:ok, bytes}
      :error -> {:error, :malformed_packet}
    end
  end
end
