defmodule Arc.Data.Agora do
  @moduledoc """
  Citizen-signed public posts and mandatory verification for Agora interfaces.

  The signing domain is deliberately limited to public posts. The board key is
  taken from the selected provider, and no secret is sent to that provider.
  """

  alias Arc.Identity

  @fields ~w(version author board body parent created_at nonce signature id)
  @domain "arc-agora-post-v1\n"
  @max_reply_bytes 1_500_000

  def enabled?(capability) do
    capability
    |> Arc.Data.InterfaceManifest.cli()
    |> case do
      %{"commands" => commands} ->
        Enum.any?(commands, &(get_in(&1, ["input", "source"]) == "agora"))

      _ ->
        false
    end
  end

  def board(tool) do
    get_in(tool, ["provider", "public_key"]) || tool["provider_public_key"]
  end

  def sign(%Identity{} = identity, board, body, parent \\ :null) do
    unsigned = %{
      "version" => 1,
      "author" => Identity.encode_public_key(identity),
      "board" => board,
      "body" => body,
      "parent" => parent,
      "created_at" => System.system_time(:second),
      "nonce" => hex(:crypto.strong_rand_bytes(16))
    }

    if valid_unsigned?(unsigned) do
      message = signing_message(unsigned)
      signature = Identity.sign(identity, message)

      {:ok,
       Map.merge(unsigned, %{
         "signature" => hex(signature),
         "id" => hex(:crypto.hash(:sha256, message <> signature))
       })}
    else
      invalid("invalid public post")
    end
  end

  def verify(post, board) when is_map(post) do
    with true <- Enum.sort(Map.keys(post)) == Enum.sort(@fields),
         true <- valid_unsigned?(post) and post["board"] == board,
         true <- hex?(post["signature"], 128) and hex?(post["id"], 64),
         {:ok, author} <- Base.decode16(post["author"], case: :lower),
         {:ok, signature} <- Base.decode16(post["signature"], case: :lower),
         message = signing_message(post),
         true <- Identity.verify(author, message, signature),
         true <- hex(:crypto.hash(:sha256, message <> signature)) == post["id"] do
      :ok
    else
      _ -> invalid("invalid Agora post signature or destination")
    end
  rescue
    _ -> invalid("invalid Agora post")
  end

  def verify(_, _), do: invalid("invalid Agora post")

  def signing_message(post) do
    @domain <>
      encode([
        post["version"],
        post["author"],
        post["board"],
        post["body"],
        post["parent"],
        post["created_at"],
        post["nonce"]
      ])
  end

  # The expected request stays local and is used to check the entire reply.
  def prepare(operation, values, context) do
    with true <- hex?(context[:board], 64),
         {:ok, request} <- request(operation, values, context) do
      {:ok, encode(request), %{board: context.board, request: request}}
    else
      false -> invalid("Agora requires a pinned provider public key")
      error -> error
    end
  end

  def finish(text, %{board: board, request: request})
      when is_binary(text) and byte_size(text) <= @max_reply_bytes do
    with %{} = result <- :json.decode(text),
         :ok <- verify_result(result, request, board) do
      # Re-encode verified data so trailing bytes or unexamined source formatting
      # cannot be presented as part of a trusted result.
      {:ok, encode(result)}
    else
      _ -> invalid("Agora returned an invalid or mismatched result")
    end
  rescue
    _ -> invalid("Agora returned invalid JSON")
  end

  def finish(_, _), do: invalid("Agora response exceeds the limit or is invalid")

  defp request(operation, values, context) when operation in ["post", "reply"] do
    body = values["body"]
    body = if is_list(body), do: Enum.join(body, " "), else: body
    parent = if operation == "reply", do: values["id"], else: :null

    with true <- operation == "post" or hex?(parent, 64),
         {:ok, post} <- sign_with_context(context, body, parent),
         :ok <- verify(post, context.board) do
      {:ok, %{"op" => "post", "post" => post}}
    else
      false -> invalid("reply requires a post id")
      error -> error
    end
  end

  defp request("read", values, _context) do
    if hex?(values["id"], 64),
      do: {:ok, %{"op" => "read", "id" => values["id"]}},
      else: invalid("read requires a post id")
  end

  defp request(operation, values, _context) when operation in ["feed", "thread"] do
    with true <- operation == "feed" or hex?(values["id"], 64),
         {:ok, limit} <- integer(values["limit"], 20, 1, 50),
         {:ok, cursor} <- integer(values["after"], :null, 1, 9_007_199_254_740_991) do
      request = %{"op" => operation, "after" => cursor, "limit" => limit}
      {:ok, if(operation == "thread", do: Map.put(request, "id", values["id"]), else: request)}
    else
      false -> invalid("thread requires a post id")
      error -> error
    end
  end

  defp request(_, _, _), do: invalid("unsupported Agora operation")

  defp sign_with_context(%{identity: %Identity{} = identity, board: board}, body, parent),
    do: sign(identity, board, body, parent)

  defp sign_with_context(%{sign_post: signer, board: board}, body, parent)
       when is_function(signer, 3), do: signer.(board, body, parent)

  defp sign_with_context(_, _, _), do: invalid("Agora posting requires a local citizen identity")

  defp verify_result(result, %{"op" => "post", "post" => expected}, board) do
    if Map.keys(result) == ["post"] and result["post"] == expected,
      do: verify(result["post"], board),
      else: :error
  end

  defp verify_result(result, %{"op" => "read", "id" => id}, board) do
    with true <- Map.keys(result) == ["post"],
         :ok <- verify(result["post"], board),
         true <- result["post"]["id"] == id,
         do: :ok
  end

  defp verify_result(result, %{"op" => op} = request, board) when op in ["feed", "thread"] do
    keys = if op == "thread", do: ~w(next post posts), else: ~w(next posts)
    parent = if op == "thread", do: request["id"], else: :null
    posts = result["posts"]
    next = result["next"]

    with true <- Enum.sort(Map.keys(result)) == keys,
         true <- is_list(posts) and length(posts) <= request["limit"],
         true <- valid_cursor?(next, request["after"], posts),
         true <- length(Enum.uniq_by(posts, & &1["id"])) == length(posts),
         true <- Enum.all?(posts, &(verify(&1, board) == :ok and &1["parent"] == parent)) do
      verify_thread_parent(op, result, request, board)
    end
  end

  defp verify_result(_, _, _), do: :error

  defp verify_thread_parent("feed", _, _, _), do: :ok

  defp verify_thread_parent("thread", result, request, board) do
    with :ok <- verify(result["post"], board),
         true <- result["post"]["id"] == request["id"],
         do: :ok
  end

  defp valid_cursor?(:null, _, _), do: true

  defp valid_cursor?(next, previous, posts),
    do:
      is_integer(next) and next > 0 and next <= 9_007_199_254_740_991 and
        posts != [] and (previous == :null or next > previous)

  defp integer(value, default, _min, _max) when value in [nil, "", :null], do: {:ok, default}

  defp integer(value, default, min, max) when is_binary(value) do
    case Integer.parse(value) do
      {number, ""} -> integer(number, default, min, max)
      _ -> invalid("invalid Agora pagination")
    end
  end

  defp integer(value, _, min, max) when is_integer(value) and value >= min and value <= max,
    do: {:ok, value}

  defp integer(_, _, _, _), do: invalid("invalid Agora pagination")

  defp valid_unsigned?(post) do
    post["version"] === 1 and hex?(post["author"], 64) and hex?(post["board"], 64) and
      valid_body?(post["body"]) and valid_parent?(post["parent"]) and
      is_integer(post["created_at"]) and post["created_at"] >= 0 and
      post["created_at"] <= 9_007_199_254_740_991 and hex?(post["nonce"], 32)
  end

  defp valid_body?(body),
    do:
      is_binary(body) and byte_size(body) <= 4096 and String.valid?(body) and
        String.trim(body) != ""

  defp valid_parent?(:null), do: true
  defp valid_parent?(parent), do: hex?(parent, 64)

  defp hex?(value, size),
    do: is_binary(value) and byte_size(value) == size and Regex.match?(~r/\A[0-9a-f]+\z/, value)

  defp hex(bytes), do: Base.encode16(bytes, case: :lower)
  defp encode(value), do: value |> :json.encode() |> IO.iodata_to_binary()
  defp invalid(message), do: {:error, {:invalid_arguments, message}}
end
