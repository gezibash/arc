defmodule Dm.Parse do
  @moduledoc """
  Parses a command line into positionals and options.

  Options are `--flag value`, `--flag "value"`, or a bare `--flag`.

  A quoted value that is a JSON string literal, as the `{{key|json}}`
  template filter renders, is decoded. That keeps quotes, newlines, and
  ` --words` inside a `--body` intact. A quoted value that is not a JSON
  string literal runs to the last quote before the next ` --flag` or the end
  of input, so hand-typed quotes inside a value still survive.

  Positionals end at the first `--flag` that stands at a token boundary. A
  positional that is a JSON string literal, as `{{key|json}}` renders, is
  decoded first, so ` --words` and newlines inside an `append` text or a
  `search` query survive.

  Empty values, the bare literal `null` (the `json` filter's rendering of an
  absent argument), and the literal "false" for booleans count as absent. The
  literal "true", as a template renders a set boolean, parses as a bare flag.

  The scanner walks the line once and never backtracks over a value, so a
  multi-megabyte `--base64` or `--body` parses in linear time. A lazy regex
  over the value hits the PCRE match limit on such input and drops the option.
  """

  @flag_re ~r/(?:^|\s)--([a-z][a-z0-9-]*)/
  @after_quote_re ~r/^(?:\s+--[a-z][a-z0-9-]*(?:\s|$)|\s*$)/
  @after_token_re ~r/^(?:\s|$)/
  @token_re ~r/^(?:"((?:[^"\\]|\\.)*)"|'([^']*)'|(\S+))/s

  @doc """
  Splits a request message into the command line and the request body.

  The command line is the first line. The request body is everything after
  the first newline, or `nil` when the message is a single line.
  """
  @spec split(String.t()) :: {String.t(), String.t() | nil}
  def split(message) when is_binary(message) do
    case String.split(message, "\n", parts: 2) do
      [header, body] -> {header, body}
      [header] -> {header, nil}
    end
  end

  @spec parse(String.t()) :: {[String.t()], %{String.t() => String.t() | true}}
  def parse(line) when is_binary(line) do
    {args, rest} = positionals(String.trim(line), [])
    {Enum.reverse(args), scan_opts(rest, %{})}
  end

  # Positionals run from the start of the line to the first `--flag` that
  # stands at a token boundary. A positional that is a JSON string literal is
  # decoded, so a ` --word` inside it cannot end the positionals. Other
  # positionals follow shell quoting.
  defp positionals(text, acc) do
    text = String.trim_leading(text)

    case text do
      "" ->
        {acc, ""}

      <<"--", c, _::binary>> when c in ?a..?z ->
        {acc, text}

      <<?", _::binary>> ->
        case json_string(text, @after_token_re) do
          {:ok, value, rest} -> positionals(rest, [value | acc])
          :error -> shell_token(text, acc)
        end

      _ ->
        shell_token(text, acc)
    end
  end

  defp shell_token(text, acc) do
    [matched | groups] = Regex.run(@token_re, text)

    value =
      case groups do
        [dq | _] when dq != "" -> unescape(dq)
        [_, sq | _] when sq != "" -> sq
        [_, _, bare] -> bare
        _ -> ""
      end

    positionals(after_offset(text, byte_size(matched)), [value | acc])
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

  # The value follows at least one whitespace character. A quoted value that
  # is a JSON string literal ending where a ` --flag` or the end of input
  # follows is decoded. Any other quoted value ends at the first quote that a
  # ` --flag` or the end of input follows. Anything else that does not start
  # with `--` is one bare token.
  defp take_value(rest) do
    case Regex.run(~r/^\s+/, rest, return: :index) do
      [{0, ws}] -> take_value(after_offset(rest, ws), rest)
      nil -> {nil, rest}
    end
  end

  defp take_value("", rest), do: {nil, rest}
  defp take_value("--" <> _, rest), do: {nil, rest}

  defp take_value(<<?", _::binary>> = text, rest) do
    case json_string(text, @after_quote_re) do
      {:ok, value, after_literal} ->
        {{:quoted, value}, after_literal}

      :error ->
        case close_quote(text) do
          {:ok, close} ->
            {{:quoted, binary_part(text, 1, close - 1)}, after_offset(text, close + 1)}

          :error ->
            bare(text, rest)
        end
    end
  end

  defp take_value(text, rest), do: bare(text, rest)

  defp bare(text, rest) do
    case Regex.run(~r/^\S+/, text, return: :index) do
      [{0, len}] -> {{:bare, binary_part(text, 0, len)}, after_offset(text, len)}
      nil -> {nil, rest}
    end
  end

  defp json_string(text, boundary_re) do
    with {:ok, len} <- json_string_length(text),
         after_literal = after_offset(text, len),
         true <- Regex.match?(boundary_re, after_literal),
         {:ok, value} <- decode_json_string(binary_part(text, 0, len)) do
      {:ok, value, after_literal}
    else
      _ -> :error
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
      {:quoted, "true"} -> Map.put(acc, key, true)
      {:bare, "true"} -> Map.put(acc, key, true)
      {:bare, "null"} -> acc
      {_, v} -> Map.put(acc, key, v)
    end
  end

  defp unescape(s), do: String.replace(s, ~r/\\(.)/, "\\1")
end
