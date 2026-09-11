defmodule Journal.KPI do
  @moduledoc "Append-only KPI records in kpi.jsonl per notebook."

  def append(path, record) do
    File.mkdir_p!(Path.dirname(path))
    File.write!(path, [IO.iodata_to_binary(:json.encode(record)), "\n"], [:append])
  end

  def read(path) do
    case File.read(path) do
      {:ok, body} ->
        body
        |> String.split("\n", trim: true)
        |> Enum.flat_map(fn line ->
          try do
            [:json.decode(line)]
          rescue
            _ -> []
          end
        end)

      _ ->
        []
    end
  end

  def log(path, key), do: path |> read() |> Enum.filter(&(&1["key"] == key))

  def latest(path) do
    path
    |> read()
    |> Enum.reduce(%{}, fn r, acc -> Map.put(acc, r["key"], r) end)
    |> Map.values()
    |> Enum.sort_by(& &1["key"])
  end

  def format(record) do
    extra =
      [{"ref", record["ref"]}, {"note", record["note"]}]
      |> Enum.reject(fn {_, v} -> v in [nil, ""] end)
      |> Enum.map_join("", fn {k, v} -> "  #{k}=#{v}" end)

    "#{record["t"]}  #{record["key"]}=#{record["value"]}  by=#{String.slice(record["by"] || "", 0, 12)}#{extra}"
  end
end
