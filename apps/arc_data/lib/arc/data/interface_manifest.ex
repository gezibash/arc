defmodule Arc.Data.InterfaceManifest do
  @moduledoc """
  Normalization and loading for declarative capability interfaces.

  Interface manifests are served on the wire as JSON under a capability's
  `"interfaces"` field. Providers can author the same structure inline as
  Elixir maps or in external JSON/TOML files.

  A command's `input.source` selects how the CLI builds the request body:

    * `"arg"` - the value of one parsed argument.
    * `"template"` - a `{{name}}` template rendered from parsed arguments.
    * `"stdin"` - the full standard input of the CLI process. If the command
      also sets `template`, the CLI renders the template first and joins it to
      the stdin body with `join_with` (default `"\n"`). Use this for large
      bodies that must not pass through the command line.
    * `"sealed_file"` - a local file sealed by the trusted CLI before upload.
    * `"private_file"` - a verified private-file get/list request. These file
      sources require interface version 3 and the CLI's paired output handler;
      the shared toolbox rejects them rather than sending unsealed arguments.
    * `"agora"` - a public post, reply, or board query. Interface version 4
      signs posts as the local citizen and requires verified responses across
      the CLI, host toolbox, and mounted MCP tool.
  """

  @cli_version 1
  @max_cli_version 4

  @doc "The newest CLI interface version this build renders."
  def max_cli_version, do: @max_cli_version

  @spec normalize(map() | nil) :: map() | nil
  def normalize(interfaces) when is_map(interfaces) do
    %{}
    |> maybe_put("cli", normalize_cli(Map.get(interfaces, "cli")))
    |> empty_map_to_nil()
  end

  def normalize(_interfaces), do: nil

  @spec from_legacy_cli(map() | nil) :: map() | nil
  def from_legacy_cli(cli) when is_map(cli) do
    case normalize_cli(cli) do
      nil -> nil
      normalized -> %{"cli" => normalized}
    end
  end

  def from_legacy_cli(_cli), do: nil

  @spec cli(map()) :: map() | nil
  def cli(%{"interfaces" => interfaces}) when is_map(interfaces) do
    case normalize(interfaces) do
      %{"cli" => cli} -> cli
      _ -> nil
    end
  end

  def cli(%{"cli" => cli}) when is_map(cli) do
    case from_legacy_cli(cli) do
      %{"cli" => normalized} -> normalized
      _ -> nil
    end
  end

  def cli(_capability), do: nil

  @spec load_file(String.t()) :: {:ok, map()} | {:error, term()}
  def load_file(path) when is_binary(path) and path != "" do
    with {:ok, body} <- File.read(path),
         {:ok, parsed} <- decode(path, body) do
      normalize_document(parsed)
    end
  end

  def load_file(_path), do: {:error, :invalid_interface_manifest}

  defp normalize_document(%{"interfaces" => interfaces}) when is_map(interfaces) do
    case normalize(interfaces) do
      %{} = normalized -> {:ok, normalized}
      _ -> {:error, :invalid_interface_manifest}
    end
  end

  defp normalize_document(%{"cli" => cli}) when is_map(cli) do
    case normalize(%{"cli" => cli}) do
      %{} = normalized -> {:ok, normalized}
      _ -> {:error, :invalid_interface_manifest}
    end
  end

  defp normalize_document(%{} = document) do
    case normalize(%{"cli" => document}) do
      %{} = normalized -> {:ok, normalized}
      _ -> {:error, :invalid_interface_manifest}
    end
  end

  defp normalize_document(_document), do: {:error, :invalid_interface_manifest}

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
        {:error, :unsupported_manifest_format}
    end
  end

  defp normalize_cli(cli) when is_map(cli) do
    namespace =
      present_string(Map.get(cli, "namespace")) ||
        present_string(Map.get(cli, "name")) ||
        present_string(get_in(cli, ["command", "name"]))

    summary =
      present_string(Map.get(cli, "summary")) ||
        present_string(get_in(cli, ["command", "summary"]))

    commands = normalize_cli_commands(Map.get(cli, "commands"), cli, summary)

    cond do
      is_nil(namespace) ->
        nil

      commands == [] ->
        nil

      true ->
        %{}
        |> Map.put("version", normalize_version(Map.get(cli, "version")))
        |> Map.put("namespace", namespace)
        |> maybe_put("summary", summary)
        |> Map.put("commands", commands)
    end
  end

  defp normalize_cli(_cli), do: nil

  defp normalize_cli_commands(commands, _cli, summary) when is_list(commands) do
    commands
    |> Enum.flat_map(&normalize_cli_command(&1, summary))
    |> unique_commands()
  end

  defp normalize_cli_commands(_commands, cli, summary) when is_map(cli) do
    case normalize_legacy_root_command(cli, summary) do
      nil -> []
      command -> [command]
    end
  end

  defp normalize_legacy_root_command(cli, summary) do
    args = normalize_cli_args(Map.get(cli, "args", []))
    input = normalize_cli_input(Map.get(cli, "input"), args)
    examples = normalize_examples(Map.get(cli, "examples", []))

    if args == [] and is_nil(input) and examples == [] and is_nil(summary) do
      nil
    else
      %{}
      |> Map.put("path", [])
      |> maybe_put("summary", summary)
      |> maybe_put("args", if(args == [], do: nil, else: args))
      |> maybe_put("input", input)
      |> maybe_put("examples", if(examples == [], do: nil, else: examples))
    end
  end

  defp normalize_cli_command(command, fallback_summary) when is_map(command) do
    path = normalize_command_path(Map.get(command, "path") || Map.get(command, "name"))
    args = normalize_cli_args(Map.get(command, "args", []))
    input = normalize_cli_input(Map.get(command, "input"), args)
    summary = present_string(Map.get(command, "summary")) || fallback_summary
    examples = normalize_examples(Map.get(command, "examples", []))
    invoke = normalize_cli_invoke(Map.get(command, "invoke"))
    output = normalize_cli_output(Map.get(command, "output"))

    if is_nil(path) do
      []
    else
      [
        %{}
        |> Map.put("path", path)
        |> maybe_put("summary", summary)
        |> maybe_put("args", if(args == [], do: nil, else: args))
        |> maybe_put("input", input)
        |> maybe_put("invoke", invoke)
        |> maybe_put("output", output)
        |> maybe_put("examples", if(examples == [], do: nil, else: examples))
      ]
    end
  end

  defp normalize_cli_command(_command, _fallback_summary), do: []

  defp normalize_command_path(nil), do: []

  defp normalize_command_path(path) when is_binary(path) do
    path
    |> String.split(~r/\s+/, trim: true)
    |> normalize_path_segments()
  end

  defp normalize_command_path(path) when is_list(path) do
    path
    |> Enum.map(&present_string/1)
    |> Enum.reject(&is_nil/1)
    |> normalize_path_segments()
  end

  defp normalize_command_path(_path), do: nil

  defp normalize_path_segments([]), do: []

  defp normalize_path_segments(segments) do
    normalized =
      segments
      |> Enum.map(fn segment ->
        segment
        |> String.downcase()
        |> String.replace(~r/[^a-z0-9-]+/u, "-")
        |> String.replace(~r/-+/u, "-")
        |> String.trim("-")
      end)
      |> Enum.reject(&(&1 == ""))

    if normalized == [], do: nil, else: normalized
  end

  defp unique_commands(commands) do
    commands
    |> Enum.reduce([], fn command, acc ->
      if Enum.any?(acc, &(Map.get(&1, "path", []) == Map.get(command, "path", []))) do
        acc
      else
        acc ++ [command]
      end
    end)
  end

  defp normalize_cli_args(args) when is_list(args) do
    args
    |> Enum.flat_map(&normalize_cli_arg/1)
    |> enforce_variadic_tail()
  end

  defp normalize_cli_args(_), do: []

  defp normalize_cli_arg(arg) when is_map(arg) do
    case Map.get(arg, "name") do
      name when is_binary(name) and name != "" ->
        kind = normalize_cli_arg_kind(Map.get(arg, "kind"))
        type = normalize_cli_arg_type(Map.get(arg, "type"))
        required = truthy?(Map.get(arg, "required"), kind == "positional")
        variadic = kind == "positional" and truthy?(Map.get(arg, "variadic"), false)

        normalized =
          %{
            "name" => name,
            "kind" => kind,
            "type" => type,
            "required" => required,
            "variadic" => variadic
          }
          |> maybe_put("description", present_string(Map.get(arg, "description")))
          |> maybe_put("flag", normalize_cli_flag(Map.get(arg, "flag"), name, kind))

        [normalized]

      _ ->
        []
    end
  end

  defp normalize_cli_arg(_), do: []

  defp normalize_cli_input(%{"source" => "agora"} = input, _args),
    do: %{"source" => "agora", "operation" => present_string(input["operation"])}

  defp normalize_cli_input(%{"source" => "sealed_file"} = input, _args),
    do: %{"source" => "sealed_file", "file" => present_string(input["file"])}

  defp normalize_cli_input(%{"source" => "private_file"} = input, _args) do
    %{
      "source" => "private_file",
      "operation" => present_string(input["operation"]),
      "id" => present_string(input["id"]),
      "after" => present_string(input["after"])
    }
  end

  defp normalize_cli_input(%{"source" => "arg", "name" => name} = input, _args)
       when is_binary(name) and name != "" do
    %{"source" => "arg", "name" => name, "join_with" => Map.get(input, "join_with", " ")}
  end

  defp normalize_cli_input(%{"source" => "template", "template" => template}, _args)
       when is_binary(template) and template != "" do
    %{"source" => "template", "template" => template}
  end

  defp normalize_cli_input(%{"source" => "json"}, _args), do: %{"source" => "json"}

  defp normalize_cli_input(%{"source" => "stdin"} = input, _args) do
    %{"source" => "stdin", "join_with" => Map.get(input, "join_with", "\n")}
    |> maybe_put("template", present_string(Map.get(input, "template")))
    |> maybe_put("seal_to", normalize_seal_to(Map.get(input, "seal_to")))
    |> maybe_put("body", present_string(Map.get(input, "body")))
    |> maybe_put("file", present_string(Map.get(input, "file")))
    |> maybe_put("attach", present_string(Map.get(input, "attach")))
  end

  defp normalize_cli_input(input, _args) when is_map(input), do: nil

  defp normalize_cli_input(_, [%{"name" => name}]) when is_binary(name) do
    %{"source" => "arg", "name" => name, "join_with" => " "}
  end

  defp normalize_cli_input(_, _args), do: nil

  defp normalize_seal_to(target) when is_binary(target), do: normalize_seal_to([target])

  defp normalize_seal_to(targets) when is_list(targets) do
    case targets |> Enum.map(&present_string/1) |> Enum.reject(&is_nil/1) do
      [] -> nil
      list -> list
    end
  end

  defp normalize_seal_to(_), do: nil

  @output_filters ~w(open petnames conversation markdown cache)

  defp normalize_cli_output(%{"private_file" => output}) when is_map(output) do
    %{
      "private_file" => %{
        "operation" => present_string(output["operation"]),
        "id" => present_string(output["id"]),
        "path" => present_string(output["path"])
      }
    }
  end

  # `output.filter` is one filter name or a list, applied in order. Known
  # filters are `open`, `petnames`, and `preview:<n>`. Unknown entries are
  # dropped. The normalized shape is always `%{"filters" => [..]}`.
  defp normalize_cli_output(%{"filter" => filter}) when is_binary(filter),
    do: normalize_cli_output(%{"filter" => [filter]})

  # Already normalized. Normalization runs again on an installed record.
  defp normalize_cli_output(%{"filters" => filters}) when is_list(filters),
    do: normalize_cli_output(%{"filter" => filters})

  defp normalize_cli_output(%{"filter" => filters}) when is_list(filters) do
    case Enum.filter(filters, &valid_output_filter?/1) do
      [] -> nil
      list -> %{"filters" => list}
    end
  end

  defp normalize_cli_output(_), do: nil

  defp valid_output_filter?(filter) when filter in @output_filters, do: true
  defp valid_output_filter?("preview:" <> n), do: Regex.match?(~r/^[1-9][0-9]*$/, n)
  defp valid_output_filter?(_), do: false

  defp normalize_cli_invoke(invoke) when is_map(invoke) do
    %{}
    |> maybe_put("mode", normalize_cli_invoke_mode(Map.get(invoke, "mode")))
    |> maybe_put("method", present_string(Map.get(invoke, "method")))
    |> maybe_put("path", present_string(Map.get(invoke, "path")))
    |> maybe_put("stream", normalize_cli_stream(Map.get(invoke, "stream")))
    |> maybe_put("topics", normalize_topics(Map.get(invoke, "topics")))
    |> empty_map_to_nil()
  end

  defp normalize_cli_invoke(_invoke), do: nil

  # Topic globs an events command listens for. `*` matches any run of
  # characters. Absent means every topic.
  defp normalize_topics(topics) when is_list(topics) do
    case topics |> Enum.map(&present_string/1) |> Enum.reject(&is_nil/1) do
      [] -> nil
      list -> list
    end
  end

  defp normalize_topics(topic) when is_binary(topic), do: normalize_topics([topic])
  defp normalize_topics(_), do: nil

  defp normalize_cli_stream(stream) when is_map(stream) do
    operations =
      case Map.get(stream, "operations") do
        list when is_list(list) ->
          list
          |> Enum.filter(&is_binary/1)
          |> Enum.map(&String.downcase/1)
          |> Enum.uniq()

        _ ->
          nil
      end

    %{}
    |> maybe_put("operations", operations)
    |> maybe_put("tty", normalize_optional_boolean(Map.get(stream, "tty")))
    |> maybe_put("encoding", present_string(Map.get(stream, "encoding")))
    |> empty_map_to_nil()
  end

  defp normalize_cli_stream(_stream), do: nil

  defp normalize_examples(examples) when is_list(examples) do
    Enum.filter(examples, &is_binary/1)
  end

  defp normalize_examples(_), do: []

  defp normalize_version(version) when is_integer(version) and version > 0, do: version
  defp normalize_version(_version), do: @cli_version

  defp maybe_put(map, _key, nil), do: map
  defp maybe_put(map, key, value), do: Map.put(map, key, value)

  defp empty_map_to_nil(map) when map_size(map) == 0, do: nil
  defp empty_map_to_nil(map), do: map

  defp present_string(value) when is_binary(value) and value != "", do: value
  defp present_string(_value), do: nil

  defp normalize_cli_arg_kind("option"), do: "option"
  defp normalize_cli_arg_kind(_), do: "positional"

  defp normalize_cli_arg_type("boolean"), do: "boolean"
  defp normalize_cli_arg_type(_), do: "string"

  defp normalize_cli_flag(_flag, _name, "positional"), do: nil

  defp normalize_cli_flag(flag, _name, "option") when is_binary(flag) and flag != "" do
    if String.starts_with?(flag, "--"), do: flag, else: "--" <> flag
  end

  defp normalize_cli_flag(_flag, name, "option"), do: "--" <> String.replace(name, "_", "-")

  defp truthy?(nil, default), do: default
  defp truthy?(value, _default) when value in [true, "true", 1, "1"], do: true
  defp truthy?(_value, _default), do: false

  defp normalize_cli_invoke_mode("stream"), do: "stream"
  defp normalize_cli_invoke_mode("events"), do: "events"
  defp normalize_cli_invoke_mode("request_reply"), do: "request_reply"
  defp normalize_cli_invoke_mode(_mode), do: nil

  defp normalize_optional_boolean(value) when value in [true, false], do: value
  defp normalize_optional_boolean(_value), do: nil

  # Only the last positional may be variadic. Options may follow it.
  defp enforce_variadic_tail(args) do
    last_positional =
      args
      |> Enum.with_index()
      |> Enum.filter(fn {arg, _} -> arg["kind"] == "positional" end)
      |> List.last()

    case last_positional do
      nil ->
        args

      {_, last_index} ->
        args
        |> Enum.with_index()
        |> Enum.map(fn
          {arg, ^last_index} -> arg
          {arg, _} -> Map.put(arg, "variadic", false)
        end)
    end
  end
end
