defmodule Journal.Parse do
  @moduledoc """
  Parses a command line into positionals and options.

  Options are `--flag value`, `--flag "value"`, or a bare `--flag`.

  A quoted value that is a JSON string literal, as the `{{key|json}}`
  template filter renders, is decoded. That keeps quotes, newlines, and
  ` --words` inside a `--body` intact. A quoted value that is not a JSON
  string literal runs to the last quote before the next ` --flag` or the end
  of input, so hand-typed quotes inside a value still survive.

  Empty values, the bare literal `null` (the `json` filter's rendering of an
  absent argument), and the literal "false" for booleans count as absent.
  """

  @flag_re ~r/^--([a-z][a-z0-9-]*)(?:\s+|$)/
  @flag_boundary_re ~r/^(?:\s+--[a-z][a-z0-9-]*(?:\s|$)|\s*$)/
  @legacy_quoted_re ~r/^("(?:.|\n)*?")(?=\s+--[a-z][a-z0-9-]*(?:\s|$)|\s*$)/

  @spec parse(String.t()) :: {[String.t()], %{String.t() => String.t() | true}}
  def parse(line) when is_binary(line) do
    line = String.trim(line)

    {head, rest} =
      case Regex.run(~r/^(.*?)((?:^|\s)--[a-z][a-z0-9-]*(?:\s|$).*)$/s, line) do
        [_, head, rest] -> {head, rest}
        nil -> {line, ""}
      end

    {tokens(head), options(rest, %{})}
  end

  defp options(rest, acc) do
    rest = String.trim_leading(rest)

    cond do
      rest == "" ->
        acc

      match = Regex.run(@flag_re, rest) ->
        [flag, key] = match
        {value, remaining} = rest |> drop(byte_size(flag)) |> option_value()
        options(remaining, put_opt(acc, key, value))

      true ->
        # A stray token where a flag was expected. Skip it.
        options(drop_token(rest), acc)
    end
  end

  defp option_value(""), do: {true, ""}

  defp option_value(rest) do
    cond do
      Regex.match?(~r/^--[a-z]/, rest) -> {true, rest}
      String.starts_with?(rest, "\"") -> quoted_value(rest)
      true -> bare_value(rest)
    end
  end

  defp bare_value(rest) do
    [value, remaining] =
      case String.split(rest, ~r/\s+/, parts: 2) do
        [value, remaining] -> [value, remaining]
        [value] -> [value, ""]
      end

    {{:bare, value}, remaining}
  end

  # Prefer a JSON string literal that ends exactly at a flag boundary. Fall
  # back to the legacy rule (last quote before the next flag) otherwise.
  defp quoted_value(rest) do
    with {:ok, len} <- json_string_length(rest),
         {literal, remaining} <- {binary_part(rest, 0, len), drop(rest, len)},
         true <- Regex.match?(@flag_boundary_re, remaining),
         {:ok, value} <- decode_json_string(literal) do
      {value, remaining}
    else
      _ -> legacy_quoted_value(rest)
    end
  end

  defp legacy_quoted_value(rest) do
    case Regex.run(@legacy_quoted_re, rest) do
      [quoted, _] -> {String.slice(quoted, 1..-2//1), drop(rest, byte_size(quoted))}
      nil -> bare_value(rest)
    end
  end

  # Byte length of the JSON string literal at the start of the binary,
  # quotes included. Escapes and quotes are ASCII, so scanning bytes is safe.
  defp json_string_length(<<?", rest::binary>>), do: json_string_end(rest, 1)

  defp json_string_end(<<?\\, _, rest::binary>>, n), do: json_string_end(rest, n + 2)
  defp json_string_end(<<?", _::binary>>, n), do: {:ok, n + 1}
  defp json_string_end(<<_, rest::binary>>, n), do: json_string_end(rest, n + 1)
  defp json_string_end(<<>>, _n), do: :error

  defp decode_json_string(literal) do
    case :json.decode(literal) do
      value when is_binary(value) -> {:ok, value}
      _ -> :error
    end
  rescue
    _ -> :error
  end

  defp put_opt(acc, key, value) do
    key = String.replace(key, "-", "_")

    case value do
      {:bare, "null"} -> acc
      {:bare, "false"} -> acc
      {:bare, v} -> Map.put(acc, key, v)
      "false" -> acc
      "" -> acc
      v -> Map.put(acc, key, v)
    end
  end

  defp drop(binary, n), do: binary_part(binary, n, byte_size(binary) - n)

  defp drop_token(rest) do
    case String.split(rest, ~r/\s+/, parts: 2) do
      [_, remaining] -> remaining
      [_] -> ""
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
