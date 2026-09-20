defmodule Arc.Data.Render.Markdown do
  @moduledoc """
  Renders `thread --bodies` output as a markdown transcript, for export.
  Same input format as `Arc.Data.Render.Conversation`.
  """

  alias Arc.Data.Render.Conversation

  @spec render(String.t()) :: String.t()
  def render(text) when is_binary(text) do
    case String.split(text, "\n") do
      [header | rest] ->
        records = rest |> Conversation.records() |> Enum.map(&Conversation.parse_record/1)

        ["# " <> header, "" | Enum.map(records, &render_record/1)]
        |> Enum.join("\n")

      _ ->
        text
    end
  end

  defp render_record(%{id: id, dir: dir, peer: peer, t: t, reply_to: reply_to, body: body} = r) do
    who = if dir == "out", do: "you", else: peer
    reply = if reply_to == "-", do: "", else: " (reply to #{reply_to})"
    body = if r.state == "retracted", do: "_(retracted)_", else: body
    attachments = Enum.map(r.attachments, fn [name, bytes] -> "- 📎 #{name} (#{bytes} bytes)" end)
    reactions = Enum.map(r.reactions, fn [value, by] -> "- #{value} #{by}" end)

    (["## #{who} — #{t}#{reply}", "", "<!-- id: #{id} -->", String.trim_trailing(body), ""] ++
       attachments ++ reactions ++ [""])
    |> Enum.join("\n")
  end

  defp render_record(raw) when is_binary(raw), do: raw
end
