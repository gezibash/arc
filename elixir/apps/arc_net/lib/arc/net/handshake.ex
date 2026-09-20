defmodule Arc.Net.Handshake do
  @moduledoc """
  Relay handshake primitives.

  Handshake v1:

    relay -> client: <<relay_pubkey::32, challenge::32>>
    client -> relay: <<client_pubkey::32, signature::64>>

  The client signature proves possession of the Ed25519 secret key:

    signature = Sign(client_sk, "ARC_RELAY_AUTH_V1" <> relay_pubkey <> challenge <> client_pubkey)

  Relay info: right after the hello, the relay sends one framed control
  message that advertises its frame cap:

    relay -> client: frame(<<"ARC_RELAY_INFO_V1", max_frame_bytes::32>>)

  `max_frame_bytes` is `0` when the relay accepts frames of any size. A client
  that predates this message drops it as an invalid packet, so the hello
  stays compatible in both directions.
  """

  alias Arc.Identity

  @relay_pubkey_bytes 32
  @challenge_bytes 32
  @client_pubkey_bytes 32
  @signature_bytes 64
  @client_hello_bytes @client_pubkey_bytes + @signature_bytes
  @proof_context "ARC_RELAY_AUTH_V1"
  @info_magic "ARC_RELAY_INFO_V1"

  @doc "Encode the relay info control message. `:unbounded` encodes as 0."
  @spec relay_info(:unbounded | pos_integer()) :: binary()
  def relay_info(:unbounded), do: <<@info_magic, 0::32-big>>

  def relay_info(max_frame_bytes) when is_integer(max_frame_bytes) and max_frame_bytes > 0,
    do: <<@info_magic, max_frame_bytes::32-big>>

  @doc "Decode a relay info control message into the advertised frame cap."
  @spec decode_relay_info(binary()) :: {:ok, :unbounded | pos_integer()} | :error
  def decode_relay_info(<<@info_magic, 0::32-big>>), do: {:ok, :unbounded}
  def decode_relay_info(<<@info_magic, max_frame_bytes::32-big>>), do: {:ok, max_frame_bytes}
  def decode_relay_info(_), do: :error

  @spec relay_hello(binary(), binary()) :: {:ok, binary()} | {:error, :invalid_relay_hello}
  def relay_hello(relay_pubkey, challenge)
      when is_binary(relay_pubkey) and is_binary(challenge) do
    if byte_size(relay_pubkey) == @relay_pubkey_bytes and byte_size(challenge) == @challenge_bytes do
      {:ok, <<relay_pubkey::binary, challenge::binary>>}
    else
      {:error, :invalid_relay_hello}
    end
  end

  @spec decode_relay_hello(binary()) ::
          {:ok, binary(), binary()} | {:error, :invalid_relay_hello}
  def decode_relay_hello(
        <<relay_pubkey::binary-size(@relay_pubkey_bytes),
          challenge::binary-size(@challenge_bytes)>>
      ) do
    {:ok, relay_pubkey, challenge}
  end

  def decode_relay_hello(_), do: {:error, :invalid_relay_hello}

  @spec client_hello(Identity.t(), binary(), binary()) ::
          {:ok, binary(), binary()} | {:error, :invalid_identity | :invalid_relay_hello}
  def client_hello(
        %Identity{public_key: pubkey, secret_key: secret_key} = identity,
        relay_pubkey,
        challenge
      )
      when is_binary(secret_key) and byte_size(secret_key) > 0 do
    if valid_pubkey?(pubkey) do
      with {:ok, _hello} <- relay_hello(relay_pubkey, challenge) do
        message = proof_message(relay_pubkey, challenge, pubkey)
        signature = Identity.sign(identity, message)
        {:ok, <<pubkey::binary, signature::binary>>, pubkey}
      end
    else
      {:error, :invalid_identity}
    end
  end

  def client_hello(_identity, _relay_pubkey, _challenge), do: {:error, :invalid_identity}

  @spec extract_client_hello(binary()) ::
          {:ok, binary(), binary(), binary()} | :more | {:error, :invalid_client_hello}
  def extract_client_hello(buffer)
      when is_binary(buffer) and byte_size(buffer) < @client_hello_bytes,
      do: :more

  def extract_client_hello(
        <<pubkey::binary-size(@client_pubkey_bytes), signature::binary-size(@signature_bytes),
          rest::binary>>
      ) do
    {:ok, pubkey, signature, rest}
  end

  def extract_client_hello(_), do: {:error, :invalid_client_hello}

  @spec verify_client_hello(binary(), binary(), binary(), binary()) :: boolean()
  def verify_client_hello(pubkey, signature, relay_pubkey, challenge)
      when is_binary(pubkey) and is_binary(signature) and is_binary(relay_pubkey) and
             is_binary(challenge) do
    if valid_pubkey?(pubkey) and byte_size(signature) == @signature_bytes and
         valid_pubkey?(relay_pubkey) and byte_size(challenge) == @challenge_bytes do
      Identity.verify(pubkey, proof_message(relay_pubkey, challenge, pubkey), signature)
    else
      false
    end
  end

  defp proof_message(relay_pubkey, challenge, pubkey) do
    @proof_context <> relay_pubkey <> challenge <> pubkey
  end

  defp valid_pubkey?(pubkey) when is_binary(pubkey), do: byte_size(pubkey) == @client_pubkey_bytes
  defp valid_pubkey?(_), do: false
end
