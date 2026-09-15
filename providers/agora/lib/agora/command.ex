defmodule Agora.Command do
  @moduledoc false
  alias Agora.{Config, Post, Store}

  def run(root, board, from, message) when is_binary(message) do
    if byte_size(message) <= Config.max_line_bytes() do
      with true <- Post.key?(from),
           %{} = request <- :json.decode(message),
           op when op in ["post", "read", "feed", "thread"] <- request["op"] do
        dispatch(root, board, from, op, request)
      else
        _ -> {:error, "invalid_request"}
      end
    else
      {:error, "invalid_request"}
    end
  rescue
    _ -> {:error, "invalid_request"}
  end

  def run(_, _, _, _), do: {:error, "invalid_request"}

  defp dispatch(root, board, from, "post", %{"post" => post}) do
    with {:ok, valid} <- Post.validate(post, board),
         true <- valid["author"] == from do
      Store.put(root, board, valid)
    else
      _ -> {:error, "invalid_post"}
    end
  end

  defp dispatch(root, board, _from, "read", %{"id" => id}), do: Store.read(root, board, id)

  defp dispatch(root, board, _from, "feed", request),
    do: Store.feed(root, board, cursor(request), limit(request))

  defp dispatch(root, board, _from, "thread", %{"id" => id} = request),
    do: Store.thread(root, board, id, cursor(request), limit(request))

  defp dispatch(_, _, _, _, _), do: {:error, "invalid_request"}

  defp cursor(request) do
    case Map.get(request, "after") do
      nil -> nil
      :null -> nil
      value when is_integer(value) and value > 0 -> value
      _ -> :invalid
    end
  end

  defp limit(request) do
    case Map.get(request, "limit", 20) do
      value when is_integer(value) and value in 1..50 -> value
      _ -> :invalid
    end
  end
end
