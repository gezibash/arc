defmodule Files.Envelope do
  @moduledoc false

  @key ~r/\A[a-f0-9]{64}\z/
  @signature ~r/\A[a-f0-9]{128}\z/
  @id ~r/\A[a-f0-9]{64}\z/
  @token ~r/\Asealed-v1:[A-Za-z0-9+\/=]+\z/

  def key?(value), do: is_binary(value) and Regex.match?(@key, value)
  def id?(value), do: is_binary(value) and Regex.match?(@id, value)

  def validate(%{} = envelope, owner) when is_binary(owner) do
    with 1 <- envelope["version"],
         true <- envelope["owner"] == owner,
         true <- key?(owner),
         name when is_binary(name) <- envelope["name"],
         body when is_binary(body) <- envelope["body"],
         true <- byte_size(name) <= Files.Config.max_name_bytes(),
         true <- byte_size(body) <= Files.Config.max_body_bytes(),
         :ok <- token?(name),
         :ok <- token?(body),
         hash when is_binary(hash) <- envelope["body_hash"],
         true <- id?(hash),
         true <- hash == sha256(body),
         signature when is_binary(signature) <- envelope["signature"],
         true <- Regex.match?(@signature, signature),
         id when is_binary(id) <- envelope["id"],
         true <- id?(id),
         message = signature_message(owner, name, hash),
         true <- id == sha256(message <> "\n" <> signature),
         :ok <- verify(owner, message, signature) do
      {:ok, canonical(envelope)}
    else
      _ -> {:error, "invalid_envelope"}
    end
  end

  def validate(_, _), do: {:error, "invalid_envelope"}

  def headers(envelope), do: Map.delete(envelope, "body")
  def sha256(value), do: :crypto.hash(:sha256, value) |> Base.encode16(case: :lower)

  def signature_message(owner, name, hash),
    do: "arc-private-file-v1\n" <> owner <> "\n" <> name <> "\n" <> hash

  defp canonical(envelope) do
    Map.take(envelope, ["version", "owner", "name", "body_hash", "signature", "id", "body"])
  end

  defp token?("sealed-v1:" <> encoded) do
    if Regex.match?(@token, "sealed-v1:" <> encoded) do
      case Base.decode64(encoded) do
        {:ok,
         <<1, _ephemeral_public_key::binary-size(32), _authentication_tag::binary-size(16),
           _ciphertext::binary>>} ->
          :ok

        _ ->
          {:error, :token}
      end
    else
      {:error, :token}
    end
  end

  defp token?(_), do: {:error, :token}

  defp verify(owner, message, signature) do
    with {:ok, public} <- Base.decode16(owner, case: :lower),
         {:ok, signed} <- Base.decode16(signature, case: :lower),
         true <- :crypto.verify(:eddsa, :none, message, signed, [public, :ed25519]) do
      :ok
    else
      _ -> {:error, :signature}
    end
  end
end
