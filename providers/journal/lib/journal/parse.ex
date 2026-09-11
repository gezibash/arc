defmodule Journal.Parse do
  @moduledoc """
  Parses a command line into positionals and options.

  Options are `--flag value`, `--flag "value with spaces"`, or a bare `--flag`.
  A quoted option value runs to the last quote before the next ` --flag` or
  the end of input, so quotes inside a `--body` survive. Empty quoted values
  and the literal "false" for booleans count as absent.

  The scanner walks the line once and never backtracks over a value, so a
  multi-megabyte `--base64` or `--body` parses in linear time. A lazy regex
  over the value hits the PCRE match limit on such input and drops the option.
  """

  @flag_re ~r/(?:^|\s)--([a-z][a-z0-9-]*)/
  @first_flag_re ~r/(?:^|\s)--[a-z][a-z0-9-]*(?:\s|$)/
  @after_quote_re ~r/^(?:\s+--[a-z][a-z0-9-]*(?:\s|$)|\s*$)/

  @spec parse(String.t()) :: {[String.t()], %{String.t() => String.t() | true}}
  def parse(line) when is_binary(line) do
    line = String.trim(line)

    head =
      case Regex.run(@first_flag_re, line, return: :index) do
        [{start, _}] -> binary_part(line, 0, start)
        nil -> line
      end

    {tokens(head), scan_opts(line, %{})}
  end

  defp scan_opts(text, acc) do
    case Regex.run(@flag_re, text, return: :index) do
      [_, {start, len}] ->
        key = binary_part(text, start, len)
        {value, rest} = take_value(after_offset(text, start + len))
        scan_opts(rest, put_opt(acc, key, value))

      nil ->
        acc
    end
  end

  # The value follows at least one whitespace character. A quoted value ends
  # at the first quote that a ` --flag` or the end of input follows. Anything
  # else that does not start with `--` is one bare token.
  defp take_value(rest) do
    case Regex.run(~r/^\s+/, rest, return: :index) do
      [{0, ws}] -> take_value(after_offset(rest, ws), rest)
      nil -> {nil, rest}
    end
  end

  defp take_value("", rest), do: {nil, rest}
  defp take_value("--" <> _, rest), do: {nil, rest}

  defp take_value(<<?", _::binary>> = text, rest) do
    case close_quote(text) do
      {:ok, close} -> {{:quoted, binary_part(text, 1, close - 1)}, after_offset(text, close + 1)}
      :error -> bare(text, rest)
    end
  end

  defp take_value(text, rest), do: bare(text, rest)

  defp bare(text, rest) do
    case Regex.run(~r/^\S+/, text, return: :index) do
      [{0, len}] -> {{:bare, binary_part(text, 0, len)}, after_offset(text, len)}
      nil -> {nil, rest}
    end
  end

  defp close_quote(text) do
    text
    |> :binary.matches("\"")
    |> Enum.drop(1)
    |> Enum.find_value(:error, fn {pos, 1} ->
      if Regex.match?(@after_quote_re, after_offset(text, pos + 1)), do: {:ok, pos}
    end)
  end

  defp after_offset(bin, offset), do: binary_part(bin, offset, byte_size(bin) - offset)

  defp put_opt(acc, key, value) do
    key = String.replace(key, "-", "_")

    case value do
      nil -> Map.put(acc, key, true)
      {:quoted, ""} -> acc
      {:quoted, "false"} -> acc
      {:bare, "false"} -> acc
      {_, v} -> Map.put(acc, key, v)
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
