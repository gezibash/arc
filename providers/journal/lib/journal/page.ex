defmodule Journal.Page do
  @moduledoc """
  A page is YAML frontmatter plus a markdown body. The journal writes the
  YAML itself for the small set of shapes it uses, and reads it with
  YamlElixir.
  """

  defstruct meta: %{}, body: ""

  @type t :: %__MODULE__{meta: map(), body: String.t()}

  def parse("---\n" <> rest) do
    case String.split(rest, "\n---\n", parts: 2) do
      [yaml, body] ->
        %__MODULE__{meta: decode_yaml(yaml), body: body}

      _ ->
        %__MODULE__{meta: %{}, body: "---\n" <> rest}
    end
  end

  def parse(text), do: %__MODULE__{meta: %{}, body: text}

  def render(%__MODULE__{meta: meta, body: body}) do
    "---\n" <> encode_yaml(meta) <> "---\n" <> body
  end

  defp decode_yaml(yaml) do
    case YamlElixir.read_from_string(yaml) do
      {:ok, map} when is_map(map) -> map
      _ -> %{}
    end
  end

  @order ~w(title created author updated tags refs attachments links)

  defp encode_yaml(meta) do
    keys = Enum.filter(@order, &Map.has_key?(meta, &1)) ++ (Map.keys(meta) -- @order)
    Enum.map_join(keys, "", fn k -> encode_kv(k, meta[k], 0) end)
  end

  defp encode_kv(key, value, indent) when is_list(value) do
    pad = String.duplicate(" ", indent)

    if value == [] do
      "#{pad}#{key}: []\n"
    else
      "#{pad}#{key}:\n" <> Enum.map_join(value, "", &encode_item(&1, indent + 2))
    end
  end

  defp encode_kv(key, value, indent) do
    "#{String.duplicate(" ", indent)}#{key}: #{scalar(value)}\n"
  end

  defp encode_item(map, indent) when is_map(map) do
    pad = String.duplicate(" ", indent)

    map
    |> Enum.sort()
    |> Enum.with_index()
    |> Enum.map_join("", fn {{k, v}, i} ->
      prefix = if i == 0, do: "#{pad}- ", else: "#{pad}  "
      "#{prefix}#{k}: #{scalar(v)}\n"
    end)
  end

  defp encode_item(value, indent), do: "#{String.duplicate(" ", indent)}- #{scalar(value)}\n"

  defp scalar(v) when is_binary(v) do
    if String.match?(v, ~r/^[A-Za-z0-9_.\/:@+-]+$/) and v != "" and
         not String.starts_with?(v, "-") do
      v
    else
      IO.iodata_to_binary(:json.encode(v))
    end
  end

  defp scalar(v) when is_boolean(v) or is_number(v), do: to_string(v)
  defp scalar(nil), do: "null"
  defp scalar(v), do: IO.iodata_to_binary(:json.encode(to_string(v)))
end
