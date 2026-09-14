defmodule Arc.Data.Render.Conversation do
  @moduledoc """
  Renders `thread --bodies` output as a conversation.

  Input: a header line, then one record per message. A record is
  `id<TAB>dir<TAB>peer<TAB>t<TAB>reply_to<TAB>flags<TAB>body`, where the
  body may run on for more lines without tabs. `dir` is `in` or `out`.

  This format is part of the dm provider's CLI interface. A change to it
  needs a manifest version bump.
  """

  @spec render(String.t()) :: String.t()
  def render(text) when is_binary(text) do
    case String.split(text, "\n") do
      [header | rest] ->
        [render_header(header) | rest |> records() |> Enum.map(&render_record/1)]
        |> Enum.join("\n")

      _ ->
        text
    end
  end

  defp render_header(header), do: "── " <> header <> " ──"

  # Fold continuation lines into the record they belong to.
  defp records(lines) do
    lines
    |> Enum.reduce([], fn line, acc ->
      case {String.contains?(line, "\t"), acc} do
        {true, _} -> [line | acc]
        {false, [prev | rest]} -> [prev <> "\n" <> line | rest]
        {false, []} -> acc
      end
    end)
    |> Enum.reverse()
  end

  defp render_record(record) do
    case String.split(record, "\t", parts: 7) do
      [id, dir, peer, t, reply_to, flags, body] ->
        who = if dir == "out", do: "you", else: peer
        reply = if reply_to == "-", do: "", else: "  ↳ reply to #{short_id(reply_to)}"
        {state, reactions, attachments} = parse_flags(flags)
        body = if state == "retracted", do: "(retracted)", else: body
        body_lines = body |> String.trim_trailing() |> String.split("\n")

        [
          "",
          "#{who}  #{time(t)}  #{short_id(id)}#{reply}",
          Enum.map(body_lines, &("  " <> &1)),
          Enum.map(attachments, fn [name, bytes] -> "  📎 #{name} (#{bytes} bytes)" end),
          reactions_line(reactions),
          receipt(dir, state)
        ]
        |> List.flatten()
        |> Enum.reject(&is_nil/1)
        |> Enum.join("\n")

      _ ->
        record
    end
  end

  # `<state>[;reaction=<value>:<by>,...][;attach=<name>:<bytes>,...]`
  defp parse_flags(flags) do
    [state | parts] = String.split(flags, ";")

    extras =
      Map.new(parts, fn part ->
        case String.split(part, "=", parts: 2) do
          [key, list] ->
            {key, list |> String.split(",") |> Enum.map(&String.split(&1, ":", parts: 2))}

          [key] ->
            {key, []}
        end
      end)

    {state, Map.get(extras, "reaction", []), Map.get(extras, "attach", [])}
  end

  defp reactions_line([]), do: nil

  defp reactions_line(reactions) do
    "  " <> Enum.map_join(reactions, "  ", fn [value, by] -> "#{value} #{by}" end)
  end

  defp receipt("out", "read"), do: "  ✓ read"
  defp receipt("out", "delivered"), do: "  ✓ delivered"
  defp receipt(_, _), do: nil

  defp short_id(id) when byte_size(id) > 8, do: binary_part(id, byte_size(id) - 6, 6)
  defp short_id(id), do: id

  defp time(<<date::binary-size(10), "T", hm::binary-size(5), _::binary>>), do: "#{date} #{hm}"
  defp time(t), do: t
end
