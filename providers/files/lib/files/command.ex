defmodule Files.Command do
  @moduledoc false
  alias Files.{Envelope, Store}

  def run(root, from, message) when is_binary(message) do
    if byte_size(message) <= Files.Config.max_line_bytes() do
      with true <- Envelope.key?(from),
           %{} = request <- :json.decode(message),
           op when op in ["put", "get", "list"] <- request["op"] do
        dispatch(root, from, op, request)
      else
        _ -> {:error, "invalid_request"}
      end
    else
      {:error, "invalid_request"}
    end
  rescue
    _ -> {:error, "invalid_request"}
  end

  def run(_, _, _), do: {:error, "invalid_request"}

  defp dispatch(root, from, "put", %{"file" => file}),
    do:
      with({:ok, envelope} <- Envelope.validate(file, from), do: Store.put(root, from, envelope))

  defp dispatch(root, from, "get", %{"id" => id}), do: Store.get(root, from, id)

  defp dispatch(root, from, "list", request) do
    cursor = if Map.get(request, "after") == :null, do: nil, else: Map.get(request, "after")
    Store.list(root, from, cursor)
  end

  defp dispatch(_, _, _, _), do: {:error, "invalid_request"}
end
