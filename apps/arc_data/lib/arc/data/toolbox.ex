defmodule Arc.Data.Toolbox do
  @moduledoc """
  Read and interpret installed ARC tools for a specific owner identity.

  The toolbox reads the same owner-scoped `tools.json` files that the CLI
  writes. This keeps the host service and the CLI aligned on one local install
  surface without giving `arc_data` a dependency on `arc_cli`.
  """

  alias Arc.Data.InterfaceManifest
  alias Arc.Identity
  alias Arc.Identity.SealedBox

  @default_dir Path.join(["~", ".config", "arc", "tools"])

  @type owner_scope :: Identity.t() | Identity.public_key()

  @spec list(owner_scope(), keyword()) :: {:ok, [map()]} | {:error, term()}
  def list(owner, opts \\ []) do
    with {:ok, document} <- load_document(owner, opts) do
      {:ok, Map.get(document, "tools", [])}
    end
  end

  @spec get(owner_scope(), String.t(), keyword()) :: {:ok, map()} | {:error, term()}
  def get(owner, command, opts \\ []) when is_binary(command) and command != "" do
    command = normalize_command(command)

    with {:ok, tools} <- list(owner, opts) do
      case Enum.find(tools, &(&1["command"] == command)) do
        nil -> {:error, :not_found}
        tool -> {:ok, tool}
      end
    end
  end

  @spec build_invocation(map(), [String.t()]) :: {:ok, map()} | {:error, term()}
  def build_invocation(tool, argv) when is_map(tool) and is_list(argv) do
    capability = tool["capability"] || %{}
    commands = cli_commands(capability)
    base_invocation = capability["invocation"] || %{}

    if commands == [] do
      {:ok, %{input: Enum.join(argv, " "), invocation: base_invocation}}
    else
      case resolve_command(capability, argv) do
        {:ok, cli_command, remaining_argv} ->
          args = Map.get(cli_command, "args", [])

          with {:ok, values} <- parse_cli_args(args, remaining_argv),
               {:ok, input} <- render_input(cli_command, args, values) do
            invocation = merge_invocation(base_invocation, Map.get(cli_command, "invoke"))
            {:ok, %{input: input, invocation: invocation, command: cli_command}}
          end

        {:error, _reason} = error ->
          error
      end
    end
  end

  def build_invocation(_tool, _argv), do: {:error, :invalid_tool}

  def cli_interface(capability) when is_map(capability) do
    InterfaceManifest.cli(capability)
  end

  def cli_commands(capability) when is_map(capability) do
    case cli_interface(capability) do
      %{"commands" => commands} when is_list(commands) -> commands
      _ -> []
    end
  end

  def root_command(capability) when is_map(capability) do
    Enum.find(cli_commands(capability), &(Map.get(&1, "path", []) == []))
  end

  def subcommands(capability) when is_map(capability) do
    Enum.reject(cli_commands(capability), &(Map.get(&1, "path", []) == []))
  end

  def command_summary(_command, capability) when is_map(capability) do
    case cli_interface(capability) do
      %{"summary" => summary} when is_binary(summary) and summary != "" ->
        summary

      _ ->
        capability["summary"] || "Installed ARC capability"
    end
  end

  def usage_from_capability(command, capability) when is_binary(command) and is_map(capability) do
    case cli_interface(capability) do
      %{"commands" => [%{"path" => []} = root]} ->
        "arc " <> command <> usage_suffix(Map.get(root, "args", []))

      %{"commands" => commands} when is_list(commands) and commands != [] ->
        "arc " <> command <> " <subcommand>"

      _ ->
        "arc " <> command <> " [input]"
    end
  end

  def command_usage(namespace, %{"args" => args} = command)
      when is_binary(namespace) and is_list(args) do
    base =
      case Map.get(command, "path", []) do
        [] -> "arc " <> namespace
        path -> "arc " <> namespace <> " " <> Enum.join(path, " ")
      end

    base <> usage_suffix(args)
  end

  def command_usage(namespace, command) when is_binary(namespace) do
    command_usage(namespace, Map.put(command, "args", []))
  end

  defp resolve_command(capability, argv) do
    commands = cli_commands(capability)
    root = root_command(capability)

    case longest_path_match(commands, argv) do
      nil when commands == [] ->
        {:ok, %{"path" => [], "args" => []}, argv}

      nil when root != nil ->
        {:ok, root, argv}

      nil when argv == [] ->
        {:error, {:invalid_arguments, "missing required subcommand"}}

      nil ->
        {:error, {:invalid_arguments, "unknown subcommand #{hd(argv)}"}}

      cli_command ->
        path = Map.get(cli_command, "path", [])
        {:ok, cli_command, Enum.drop(argv, length(path))}
    end
  end

  defp longest_path_match(commands, argv) do
    commands
    |> Enum.filter(fn cli_command ->
      path = Map.get(cli_command, "path", [])
      path != [] and prefix_match?(argv, path)
    end)
    |> Enum.sort_by(&length(Map.get(&1, "path", [])), :desc)
    |> List.first()
  end

  defp prefix_match?(argv, path) when length(argv) < length(path), do: false
  defp prefix_match?(argv, path), do: Enum.take(argv, length(path)) == path

  defp parse_cli_args(args, argv) do
    option_specs =
      args
      |> Enum.filter(&(&1["kind"] == "option"))
      |> Map.new(fn arg -> {arg["flag"], arg} end)

    positional_specs = Enum.filter(args, &(&1["kind"] == "positional"))

    with {:ok, values, positional_tokens} <- collect_option_args(argv, option_specs, %{}, []) do
      assign_positionals(positional_specs, positional_tokens, values)
    end
  end

  defp collect_option_args([], _option_specs, values, positional_tokens) do
    {:ok, values, Enum.reverse(positional_tokens)}
  end

  defp collect_option_args([token | rest], option_specs, values, positional_tokens) do
    cond do
      String.starts_with?(token, "--") and Map.has_key?(option_specs, token) ->
        spec = Map.fetch!(option_specs, token)

        case spec["type"] do
          "boolean" ->
            collect_option_args(
              rest,
              option_specs,
              Map.put(values, spec["name"], true),
              positional_tokens
            )

          _ ->
            case rest do
              [value | tail] ->
                collect_option_args(
                  tail,
                  option_specs,
                  Map.put(values, spec["name"], value),
                  positional_tokens
                )

              [] ->
                {:error, {:invalid_arguments, "missing value for #{token}"}}
            end
        end

      String.starts_with?(token, "--") ->
        {:error, {:invalid_arguments, "unknown option #{token}"}}

      true ->
        collect_option_args(rest, option_specs, values, [token | positional_tokens])
    end
  end

  defp assign_positionals([], [], values), do: {:ok, values}

  defp assign_positionals([], _tokens, _values) do
    {:error, {:invalid_arguments, "too many positional arguments"}}
  end

  defp assign_positionals([spec | rest], tokens, values) do
    cond do
      spec["variadic"] == true ->
        case tokens do
          [] ->
            if spec["required"] do
              {:error, {:invalid_arguments, "missing required argument #{spec["name"]}"}}
            else
              {:ok, values}
            end

          _ ->
            {:ok, Map.put(values, spec["name"], tokens)}
        end

      tokens == [] and spec["required"] ->
        {:error, {:invalid_arguments, "missing required argument #{spec["name"]}"}}

      tokens == [] ->
        assign_positionals(rest, [], values)

      true ->
        [token | tail] = tokens
        assign_positionals(rest, tail, Map.put(values, spec["name"], token))
    end
  end

  @template_placeholder ~r/\{\{([a-zA-Z0-9_-]+)(?:\|([a-zA-Z0-9_]+)(?::([a-zA-Z0-9_-]+))?)?\}\}/
  @template_filters ~w(json shell pubkey seal)
  @sealed_prefix "sealed-v1:"
  @sealed_token ~r/sealed-v1:([A-Za-z0-9+\/=]+)/

  @typedoc """
  Context for filters that need more than the parsed values.

    * `:resolve` maps a name or key prefix to control plane entries, with
      the shape of `Arc.Control.resolve/1`.
    * `:identity` is the caller's identity, used by `seal:me` and `open`.
  """
  @type filter_context :: %{
          optional(:resolve) => (String.t() -> {:ok, [map()]} | {:error, term()}),
          optional(:identity) => Identity.t() | nil,
          optional(:lists) => (String.t() -> [String.t()] | nil)
        }

  @doc """
  Render the provider input for one resolved CLI command.

  `input.source` selects the renderer:

    * `"arg"` renders one named argument.
    * `"template"` substitutes `{{key}}` placeholders. A `{{key|json}}`
      placeholder renders the value as a JSON literal and `{{key|shell}}`
      renders it single-quoted for a POSIX shell. `{{key|pubkey}}` resolves
      a name to a hex public key. `{{key|seal:to}}` seals the value to the
      X25519 key of the peer named by argument `to`.
    * `"json"` renders every parsed argument as one JSON object, so the
      provider never parses a command line.
  """
  @spec render_input(map(), [map()], map(), filter_context()) ::
          {:ok, String.t()} | {:error, term()}
  def render_input(cli_command, args, values, context \\ %{}) do
    case Map.get(cli_command, "input") do
      %{"source" => "arg", "name" => name} = input_spec ->
        case Map.fetch(values, name) do
          {:ok, value} -> {:ok, render_value(value, input_spec["join_with"] || " ")}
          :error -> {:error, {:invalid_arguments, "missing required argument #{name}"}}
        end

      %{"source" => "template", "template" => template} ->
        render_template(template, values, context)

      %{"source" => "json"} ->
        {:ok, encode_json(values)}

      _ ->
        case args do
          [%{"name" => name}] ->
            case Map.fetch(values, name) do
              {:ok, value} -> {:ok, render_value(value, " ")}
              :error -> {:error, {:invalid_arguments, "missing required argument #{name}"}}
            end

          _ ->
            {:ok, ""}
        end
    end
  end

  @doc """
  Render one `{{key}}` template against parsed argument values.
  """
  @spec render_template(String.t(), map(), filter_context()) ::
          {:ok, String.t()} | {:error, term()}
  def render_template(template, values, context \\ %{}) do
    placeholders =
      @template_placeholder
      |> Regex.scan(template)
      |> Enum.map(fn
        [raw, key] -> {raw, key, "", nil}
        [raw, key, filter] -> {raw, key, filter, nil}
        [raw, key, filter, arg] -> {raw, key, filter, arg}
      end)

    with :ok <- check_filters(placeholders),
         {:ok, rendered} <- render_placeholders(placeholders, values, context) do
      result =
        Enum.reduce(rendered, template, fn {raw, text}, acc ->
          String.replace(acc, raw, text)
        end)

      {:ok, String.trim(result)}
    end
  end

  defp check_filters(placeholders) do
    case Enum.find(placeholders, fn {_, _, f, _} -> f != "" and f not in @template_filters end) do
      nil -> :ok
      {_, _, filter, _} -> {:error, {:invalid_template, "unknown filter #{filter}"}}
    end
  end

  defp render_placeholders(placeholders, values, context) do
    Enum.reduce_while(placeholders, {:ok, []}, fn {raw, key, filter, arg}, {:ok, acc} ->
      case render_placeholder(filter, arg, Map.get(values, key), values, context) do
        {:ok, text} -> {:cont, {:ok, [{raw, text} | acc]}}
        {:error, _} = error -> {:halt, error}
      end
    end)
  end

  defp render_placeholder("", _arg, value, _values, _context), do: {:ok, render_value(value, " ")}
  defp render_placeholder("json", _arg, value, _values, _context), do: {:ok, encode_json(value)}
  defp render_placeholder("shell", _arg, value, _values, _context), do: {:ok, shell_quote(value)}

  # A peer value may be a comma-separated set, and each item may name a
  # saved list. The result is one hex key per peer, comma-separated.
  defp render_placeholder("pubkey", _arg, value, _values, context) do
    with {:ok, entries} <- resolve_peers(render_value(value, " "), context) do
      {:ok, Enum.map_join(entries, ",", &Base.encode16(&1.public_key, case: :lower))}
    end
  end

  defp render_placeholder("seal", nil, _value, _values, _context) do
    {:error, {:invalid_template, "seal needs a target argument, as in {{body|seal:to}}"}}
  end

  defp render_placeholder("seal", arg, value, values, context) do
    with {:ok, tokens} <- seal_to(render_value(value, " "), arg, values, context) do
      {:ok, Enum.join(tokens, ",")}
    end
  end

  @doc """
  Seal `body` to every peer named by argument `target`, or to the caller
  when `target` is `"me"`. The argument value may be a comma-separated set
  of peers or saved lists. Returns one `sealed-v1:<base64>` token per peer,
  in order.
  """
  @spec seal_to(binary(), String.t(), map(), filter_context()) ::
          {:ok, [String.t()]} | {:error, term()}
  def seal_to(body, "me", _values, %{identity: %Identity{} = id}) do
    {x_pub, _} = Identity.to_x25519(id)
    {:ok, [encode_sealed(SealedBox.seal(x_pub, body))]}
  end

  def seal_to(_body, "me", _values, _context), do: {:error, {:no_identity, "me"}}

  def seal_to(body, target, values, context) do
    query = render_value(Map.get(values, target), " ")

    with {:ok, entries} <- resolve_peers(query, context) do
      Enum.reduce_while(entries, {:ok, []}, fn entry, {:ok, acc} ->
        case entry.x25519_public do
          <<x_pub::binary-size(32)>> ->
            {:cont, {:ok, [encode_sealed(SealedBox.seal(x_pub, body)) | acc]}}

          _ ->
            {:halt, {:error, {:no_keyex, entry.name}}}
        end
      end)
      |> case do
        {:ok, tokens} -> {:ok, Enum.reverse(tokens)}
        error -> error
      end
    end
  end

  @doc """
  Split a peer value on commas, expand saved lists through `context.lists`,
  and resolve each peer to a control plane entry. Duplicates are dropped.
  """
  @spec resolve_peers(String.t(), filter_context()) :: {:ok, [map()]} | {:error, term()}
  def resolve_peers(value, context) do
    lists = Map.get(context, :lists)

    peers =
      value
      |> String.split(",", trim: true)
      |> Enum.map(&String.trim/1)
      |> Enum.reject(&(&1 == ""))
      |> Enum.flat_map(fn item ->
        case is_function(lists, 1) && lists.(item) do
          members when is_list(members) -> members
          _ -> [item]
        end
      end)
      |> Enum.uniq()

    if peers == [] do
      {:error, {:resolve, value, :empty}}
    else
      Enum.reduce_while(peers, {:ok, []}, fn peer, {:ok, acc} ->
        case resolve_entry(peer, context) do
          {:ok, entry} -> {:cont, {:ok, [entry | acc]}}
          {:error, _} = error -> {:halt, error}
        end
      end)
      |> case do
        {:ok, entries} -> {:ok, entries |> Enum.reverse() |> Enum.uniq_by(& &1.public_key)}
        error -> error
      end
    end
  end

  @doc """
  Replace every `sealed-v1:<base64>` token in `text` with its plaintext,
  opened with `identity`. A token that does not open is replaced with
  `[sealed: cannot open]`.
  """
  @spec open_tokens(String.t(), Identity.t()) :: String.t()
  def open_tokens(text, %Identity{} = identity) when is_binary(text) do
    Regex.replace(@sealed_token, text, fn _, b64 ->
      with {:ok, sealed} <- Base.decode64(b64),
           {:ok, plain} <- SealedBox.open(identity, sealed) do
        plain
      else
        _ -> "[sealed: cannot open]"
      end
    end)
  end

  @hex_pubkey ~r/\b[a-f0-9]{64}\b/

  @doc """
  Replace every 64 hex public key in `text` with its petname. Petnames are
  deterministic, so this needs no lookup.
  """
  @spec petnames(String.t()) :: String.t()
  def petnames(text) when is_binary(text) do
    Regex.replace(@hex_pubkey, text, fn hex ->
      case Base.decode16(hex, case: :lower) do
        {:ok, pk} -> Identity.name(pk)
        :error -> hex
      end
    end)
  end

  @doc """
  Cut the last tab field of each record to `n` characters on one line,
  ending in an ellipsis when cut. A record is a line with tabs plus every
  following line without tabs, since an opened body may span lines. Lines
  before the first record, such as a header, are unchanged.
  """
  @spec preview(String.t(), pos_integer()) :: String.t()
  def preview(text, n) when is_binary(text) and is_integer(n) and n > 0 do
    text
    |> String.split("\n")
    |> Enum.reduce([], fn line, acc ->
      case {String.contains?(line, "\t"), acc} do
        {false, [prev | rest]} when is_tuple(prev) ->
          [{elem(prev, 0), elem(prev, 1) <> "\n" <> line} | rest]

        {false, _} ->
          [line | acc]

        {true, _} ->
          [{line, ""} | acc]
      end
    end)
    |> Enum.reverse()
    |> Enum.map_join("\n", fn
      {line, tail} ->
        {head, [last]} = line |> String.split("\t") |> Enum.split(-1)
        Enum.join(head ++ [truncate(last <> tail, n)], "\t")

      line ->
        line
    end)
  end

  defp truncate(text, n) do
    one_line = text |> String.replace(~r/\s*\n\s*/, " ") |> String.trim()

    if String.length(one_line) > n do
      String.slice(one_line, 0, max(n - 1, 0)) <> "…"
    else
      one_line
    end
  end

  @doc """
  Apply a command's output filters, in order, to reply text. `identity` is
  needed by `open`; without one, `open` leaves tokens as they are.
  """
  @spec apply_output_filters(String.t(), [String.t()], Identity.t() | nil) :: String.t()
  def apply_output_filters(text, filters, identity) when is_binary(text) and is_list(filters) do
    Enum.reduce(filters, text, fn
      "open", acc -> if(match?(%Identity{}, identity), do: open_tokens(acc, identity), else: acc)
      "petnames", acc -> petnames(acc)
      "preview:" <> n, acc -> preview(acc, String.to_integer(n))
      "conversation", acc -> Arc.Data.Render.Conversation.render(acc)
      _other, acc -> acc
    end)
  end

  defp encode_sealed(sealed), do: @sealed_prefix <> Base.encode64(sealed)

  defp resolve_entry(value, %{resolve: resolve}) when is_function(resolve, 1) do
    case resolve.(value) do
      {:ok, [entry]} -> {:ok, entry}
      {:ok, []} -> bare_hex_entry(value, :not_found)
      {:ok, _many} -> {:error, {:resolve, value, :ambiguous}}
      {:error, reason} -> {:error, {:resolve, value, reason}}
    end
  end

  defp resolve_entry(value, _context), do: bare_hex_entry(value, :no_resolver)

  # A full hex public key stands on its own for `pubkey`. It carries no
  # X25519 key, so `seal` still needs the control plane entry.
  defp bare_hex_entry(<<hex::binary-size(64)>> = value, reason) do
    case Base.decode16(hex, case: :mixed) do
      {:ok, pk} -> {:ok, %{public_key: pk, x25519_public: nil, name: value}}
      :error -> {:error, {:resolve, value, reason}}
    end
  end

  defp bare_hex_entry(value, reason), do: {:error, {:resolve, value, reason}}

  defp render_value(value, join_with) when is_list(value), do: Enum.join(value, join_with)
  defp render_value(true, _join_with), do: "true"
  defp render_value(false, _join_with), do: "false"
  defp render_value(nil, _join_with), do: ""
  defp render_value(value, _join_with), do: to_string(value)

  defp encode_json(nil), do: "null"
  defp encode_json(values) when is_list(values), do: encode_json(Enum.join(values, " "))
  defp encode_json(value), do: value |> :json.encode() |> IO.iodata_to_binary()

  defp shell_quote(values) when is_list(values), do: Enum.map_join(values, " ", &shell_quote/1)
  defp shell_quote(nil), do: "''"
  defp shell_quote(true), do: "true"
  defp shell_quote(false), do: "false"

  defp shell_quote(value) do
    "'" <> String.replace(to_string(value), "'", "'\\''") <> "'"
  end

  defp merge_invocation(base, override) when is_map(override), do: Map.merge(base, override)
  defp merge_invocation(base, _override), do: base

  defp usage_suffix([]), do: ""
  defp usage_suffix(args), do: " " <> Enum.map_join(args, " ", &usage_token/1)

  defp usage_token(%{
         "name" => name,
         "kind" => "option",
         "flag" => flag,
         "type" => "boolean",
         "required" => required
       }) do
    token = flag || "--" <> String.replace(name, "_", "-")
    if required, do: token, else: "[" <> token <> "]"
  end

  defp usage_token(%{"name" => name, "kind" => "option", "flag" => flag, "required" => required}) do
    token = (flag || "--" <> String.replace(name, "_", "-")) <> " <" <> name <> ">"
    if required, do: token, else: "[" <> token <> "]"
  end

  defp usage_token(%{"name" => name, "variadic" => true, "required" => required}) do
    token = "<" <> name <> "...>"
    if required, do: token, else: "[" <> token <> "]"
  end

  defp usage_token(%{"name" => name, "required" => required}) do
    token = "<" <> name <> ">"
    if required, do: token, else: "[" <> token <> "]"
  end

  defp usage_token(_arg), do: "[input]"

  defp normalize_command(value) when is_binary(value) do
    value
    |> String.downcase()
    |> String.replace(~r/[^a-z0-9-]+/u, "-")
    |> String.replace(~r/-+/u, "-")
    |> String.trim("-")
  end

  defp load_document(owner, opts) do
    with {:ok, path} <- owner_path(owner, opts) do
      case File.read(path) do
        {:ok, content} ->
          try do
            case :json.decode(content) do
              %{} = document -> {:ok, document}
              _ -> {:error, :invalid_registry}
            end
          rescue
            _ -> {:error, :invalid_registry}
          end

        {:error, :enoent} ->
          {:ok, %{"tools" => []}}

        {:error, reason} ->
          {:error, reason}
      end
    end
  end

  defp registry_root(opts) do
    opts
    |> Keyword.get(:dir, Application.get_env(:arc_cli, :tool_registry_dir, @default_dir))
    |> Path.expand()
  end

  defp owner_path(owner, opts) do
    with {:ok, owner_pk} <- owner_public_key(owner) do
      {:ok,
       Path.join([
         registry_root(opts),
         Base.encode16(owner_pk, case: :lower),
         "tools.json"
       ])}
    end
  end

  defp owner_public_key(%Identity{public_key: owner_pk}), do: {:ok, owner_pk}
  defp owner_public_key(<<owner_pk::binary-size(32)>>), do: {:ok, owner_pk}
  defp owner_public_key(_owner), do: {:error, :invalid_owner}
end
