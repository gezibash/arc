defmodule Arc.Data.CapabilityFile do
  @moduledoc """
  Load provider-authored capability definitions from JSON or TOML files.

  Files may contain either a raw capability document or a top-level
  `"capability"` table/object. In both cases the returned map is the raw
  capability payload consumed by `Arc.Data.Handler.capability/2`.
  """

  @spec load_file(String.t()) :: {:ok, map()} | {:error, term()}
  def load_file(path) when is_binary(path) and path != "" do
    with {:ok, body} <- File.read(path),
         {:ok, parsed} <- decode(path, body),
         {:ok, capability} <- extract_capability(parsed) do
      {:ok, capability}
    end
  end

  def load_file(_path), do: {:error, :invalid_capability_file}

  defp decode(path, body) do
    case Path.extname(path) do
      ".json" ->
        try do
          {:ok, :json.decode(body)}
        rescue
          _ -> {:error, :invalid_json}
        end

      ".toml" ->
        TomlElixir.decode(body)

      _ ->
        {:error, :unsupported_capability_format}
    end
  end

  defp extract_capability(%{"capability" => capability} = document) when is_map(capability) do
    capability =
      capability
      |> merge_optional("interfaces", Map.get(document, "interfaces"))
      |> merge_optional("examples", Map.get(document, "examples"))

    {:ok, capability}
  end

  defp extract_capability(%{} = capability), do: {:ok, capability}
  defp extract_capability(_document), do: {:error, :invalid_capability_file}

  defp merge_optional(map, _key, nil), do: map
  defp merge_optional(map, key, value), do: Map.put_new(map, key, value)
end
