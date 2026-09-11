defmodule Journal.Parse do
  @moduledoc """
  Parses a command line into positionals and options.

  Options are `--flag value`, `--flag "value with spaces"`, or a bare `--flag`.
  A quoted option value runs to the last quote before the next ` --flag` or
  the end of input, so quotes inside a `--body` survive. Empty quoted values
  and the literal "false" for booleans count as absent.
  """

  @opt_re ~r/(?:^|\s)--([a-z][a-z0-9-]*)(?:\s+("(?:.|\n)*?")(?=\s+--[a-z][a-z0-9-]*(?:\s|$)|\s*$)|\s+(?!--)(\S+))?/

  @spec parse(String.t()) :: {[String.t()], %{String.t() => String.t() | true}}
  def parse(line) when is_binary(line) do
    line = String.trim(line)

    head =
      case Regex.run(~r/^(.*?)(?=(?:^|\s)--[a-z][a-z0-9-]*(?:\s|$))/s, line) do
        [_, prefix] -> prefix
        nil -> line
      end

    opts =
      @opt_re
      |> Regex.scan(line)
      |> Enum.reduce(%{}, fn
        [_, key, quoted, bare], acc -> put_opt(acc, key, quoted, bare)
        [_, key, quoted], acc -> put_opt(acc, key, quoted, "")
        [_, key], acc -> put_opt(acc, key, "", "")
      end)

    {tokens(head), opts}
  end

  defp put_opt(acc, key, quoted, bare) do
    key = String.replace(key, "-", "_")

    value =
      cond do
        quoted != "" -> quoted |> String.slice(1..-2//1)
        bare != "" -> bare
        true -> true
      end

    case value do
      "false" -> acc
      "" -> acc
      v -> Map.put(acc, key, v)
    end
  end

  @doc "Shell-like split of the positional prefix, honoring double and single quotes."
  def tokens(text) do
    ~r/"((?:[^"\\]|\\.)*)"|'([^']*)'|(\S+)/s
    |> Regex.scan(text)
    |> Enum.map(fn
      [_, dq | _] when dq != "" -> unescape(dq)
      [_, _, sq | _] when sq != "" -> sq
      [_, _, _, bare] -> bare
      _ -> ""
    end)
  end

  defp unescape(s), do: String.replace(s, ~r/\\(.)/, "\\1")

  @doc "Turn a literal backslash-n sequence into a newline. Agents pass bodies on one line."
  def unescape_newlines(s), do: String.replace(s, "\\n", "\n")
end
