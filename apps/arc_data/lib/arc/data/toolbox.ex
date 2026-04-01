defmodule Arc.Data.Toolbox do
  @moduledoc """
  Read and interpret installed ARC tools for a specific owner identity.

  The toolbox reads the same owner-scoped `tools.json` files that the CLI
  writes. This keeps the host service and the CLI aligned on one local install
  surface without giving `arc_data` a dependency on `arc_cli`.
  """

  alias Arc.Data.InterfaceManifest
  alias Arc.Identity

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

    with {:ok, values, positional_tokens} <- collect_option_args(argv, option_specs, %{}, []),
         {:ok, values} <- assign_positionals(positional_specs, positional_tokens, values) do
      {:ok, values}
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

  defp render_input(cli_command, args, values) do
    case Map.get(cli_command, "input") do
      %{"source" => "arg", "name" => name} = input_spec ->
        case Map.fetch(values, name) do
          {:ok, value} -> {:ok, render_value(value, input_spec["join_with"] || " ")}
          :error -> {:error, {:invalid_arguments, "missing required argument #{name}"}}
        end

      %{"source" => "template", "template" => template} ->
        rendered =
          Regex.replace(~r/\{\{([a-zA-Z0-9_-]+)\}\}/, template, fn _, key ->
            render_value(Map.get(values, key, ""), " ")
          end)
          |> String.trim()

        {:ok, rendered}

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

  defp render_value(value, join_with) when is_list(value), do: Enum.join(value, join_with)
  defp render_value(true, _join_with), do: "true"
  defp render_value(false, _join_with), do: "false"
  defp render_value(nil, _join_with), do: ""
  defp render_value(value, _join_with), do: to_string(value)

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
