defmodule Agora.Post do
  @moduledoc false

  @key ~r/\A[a-f0-9]{64}\z/
  @signature ~r/\A[a-f0-9]{128}\z/
  @id ~r/\A[a-f0-9]{64}\z/
  @nonce ~r/\A[a-f0-9]{32}\z/
  @fields [
    "author",
    "board",
    "body",
    "created_at",
    "id",
    "nonce",
    "parent",
    "signature",
    "version"
  ]

  def key?(value), do: is_binary(value) and Regex.match?(@key, value)
  def id?(value), do: is_binary(value) and Regex.match?(@id, value)

  def validate(%{} = post, board) when is_binary(board) do
    with true <- Map.keys(post) |> Enum.sort() == @fields,
         1 <- post["version"],
         author when is_binary(author) <- post["author"],
         true <- key?(author),
         true <- post["board"] == board and key?(board),
         body when is_binary(body) <- post["body"],
         true <- String.valid?(body) and byte_size(body) <= 4096 and String.trim(body) != "",
         parent <- post["parent"],
         true <- parent == :null or id?(parent),
         created_at
         when is_integer(created_at) and created_at >= 0 and created_at <= 9_007_199_254_740_991 <-
           post["created_at"],
         nonce when is_binary(nonce) <- post["nonce"],
         true <- Regex.match?(@nonce, nonce),
         signature when is_binary(signature) <- post["signature"],
         true <- Regex.match?(@signature, signature),
         id when is_binary(id) <- post["id"],
         true <- id?(id),
         message = signature_message(author, board, body, parent, created_at, nonce),
         {:ok, raw_signature} <- Base.decode16(signature, case: :lower),
         true <- id == sha256(message <> raw_signature),
         {:ok, public} <- Base.decode16(author, case: :lower),
         true <- :crypto.verify(:eddsa, :none, message, raw_signature, [public, :ed25519]) do
      {:ok, canonical(post)}
    else
      _ -> {:error, "invalid_post"}
    end
  end

  def validate(_, _), do: {:error, "invalid_post"}

  def signature_message(author, board, body, parent, created_at, nonce) do
    encoded =
      :json.encode([1, author, board, body, parent, created_at, nonce]) |> IO.iodata_to_binary()

    "arc-agora-post-v1\n" <> encoded
  end

  def sha256(value), do: :crypto.hash(:sha256, value) |> Base.encode16(case: :lower)
  defp canonical(post), do: Map.take(post, @fields)
end
