defmodule Arc.CLI.ToolRegistry do
  @moduledoc """
  Owner-scoped installed ARC capability packages.

  Installs are verified local projections of remote capability packages. The
  stored record is the consumer-side binding: command name, provider, signer,
  hash, release channel, and the locally accepted interface contract.
  """

  alias Arc.Data.CapabilityPackage
  alias Arc.Data.InterfaceManifest
  alias Arc.Identity

  @default_dir Path.join(["~", ".config", "arc", "tools"])
  @registry_version 2
  @reserved_commands MapSet.new([
                       "keys",
                       "publish",
                       "resolve",
                       "apps",
                       "host",
                       "discover",
                       "mount",
                       "mcp",
                       "send",
                       "info",
                       "listen",
                       "serve",
                       "relay",
                       "install",
                       "tool",
                       "trust",
                       "help"
                     ])

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

  @spec install(owner_scope(), map(), keyword()) :: {:ok, map()} | {:error, term()}
  def install(owner, detail, opts \\ []) when is_map(detail) do
    with {:ok, verified} <- CapabilityPackage.verify(detail),
         {:ok, document} <- load_document(owner, opts),
         {:ok, owner_document} <- owner_document(owner),
         {:ok, command} <- command_name(verified, opts),
         :ok <- validate_install_target(document, verified, command, opts) do
      install = normalize_install(owner_document, verified, command, opts)
      tools = upsert_tool(Map.get(document, "tools", []), install)
      document = owner_document |> Map.put("version", @registry_version) |> Map.put("tools", tools)

      with :ok <- persist_document(owner, document, opts) do
        {:ok, install}
      end
    end
  end

  @spec uninstall(owner_scope(), String.t(), keyword()) :: :ok | {:error, term()}
  def uninstall(owner, command, opts \\ []) when is_binary(command) and command != "" do
    command = normalize_command(command)

    with {:ok, document} <- load_document(owner, opts) do
      tools = document |> Map.get("tools", []) |> Enum.reject(&(&1["command"] == command))
      persist_document(owner, %{document | "tools" => tools}, opts)
    end
  end

  @spec set_pinned(owner_scope(), String.t(), boolean(), keyword()) :: {:ok, map()} | {:error, term()}
  def set_pinned(owner, command, pinned?, opts \\ [])
      when is_binary(command) and is_boolean(pinned?) do
    command = normalize_command(command)

    with {:ok, document} <- load_document(owner, opts),
         {:ok, tool} <- get(owner, command, opts) do
      updated = Map.put(tool, "pinned", pinned?)
      tools = upsert_tool(Map.get(document, "tools", []), updated)
      document = Map.put(document, "tools", tools)

      with :ok <- persist_document(owner, document, opts) do
        {:ok, updated}
      end
    end
  end

  @spec installed?(owner_scope(), String.t(), keyword()) :: boolean()
  def installed?(owner, command, opts \\ []) when is_binary(command) and command != "" do
    match?({:ok, _tool}, get(owner, command, opts))
  end

  @spec command_name(map(), keyword()) :: {:ok, String.t()} | {:error, term()}
  def command_name(%{"capability" => capability}, opts) when is_map(capability) do
    case cli_command(capability) do
      nil ->
        {:error, :not_installable}

      suggested ->
        requested =
          case Keyword.get(opts, :command) do
            value when is_binary(value) and value != "" -> value
            _ -> suggested
          end

        command = normalize_command(requested)

        if command == "" do
          {:error, :invalid_command}
        else
          {:ok, command}
        end
    end
  end

  @spec reserved_command?(String.t()) :: boolean()
  def reserved_command?(command) when is_binary(command) do
    MapSet.member?(@reserved_commands, normalize_command(command))
  end

  @spec to_signed_package(map()) :: map()
  def to_signed_package(tool) when is_map(tool) do
    %{
      "package_version" => tool["package_version"],
      "published_at" => tool["published_at"],
      "provider" => tool["provider"],
      "capability" => tool["capability"],
      "release" => %{
        "version" => tool["release_version"],
        "channel" => tool["channel"]
      },
      "package_hash" => tool["package_hash"],
      "signature" => tool["signature"]
    }
  end

  def usage_from_capability(command, capability) when is_binary(command) and is_map(capability) do
    case InterfaceManifest.cli(capability) do
      %{"commands" => [%{"path" => []} = root]} ->
        "arc " <> command <> usage_suffix(Map.get(root, "args", []))

      %{"commands" => commands} when is_list(commands) and commands != [] ->
        "arc " <> command <> " <subcommand>"

      _ ->
        "arc " <> command <> " [input]"
    end
  end

  def cli_interface(capability) when is_map(capability) do
    InterfaceManifest.cli(capability)
  end

  def cli_commands(capability) when is_map(capability) do
    case cli_interface(capability) do
      %{"commands" => commands} when is_list(commands) -> commands
      _ -> []
    end
  end

  def command_summary(_command, capability) when is_map(capability) do
    case cli_interface(capability) do
      %{"summary" => summary} when is_binary(summary) and summary != "" ->
        summary

      _ ->
        capability["summary"] || "Installed ARC capability"
    end
  end

  def command_label(%{"path" => []}), do: "(root)"

  def command_label(%{"path" => path}) when is_list(path) do
    Enum.join(path, " ")
  end

  def command_label(_command), do: "(root)"

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

  def root_command(capability) when is_map(capability) do
    Enum.find(cli_commands(capability), &(Map.get(&1, "path", []) == []))
  end

  def subcommands(capability) when is_map(capability) do
    Enum.reject(cli_commands(capability), &(Map.get(&1, "path", []) == []))
  end

  def default_usage(command, capability) when is_binary(command) and is_map(capability) do
    case root_command(capability) do
      nil -> usage_from_capability(command, capability)
      root -> command_usage(command, root)
    end
  end

  def namespace_required?(capability) when is_map(capability) do
    subcommands(capability) != []
  end

  defp normalize_install(owner_document, verified, command, opts) do
    provider = verified["provider"] || %{}
    capability = verified["capability"] || %{}
    provider_name = provider["name"] || provider["short_name"] || "unknown"
    capability_id = capability["id"] || "unknown"

    %{
      "command" => command,
      "install_id" => provider_name <> "/" <> capability_id,
      "owner_name" => owner_document["owner_name"],
      "owner_public_key" => owner_document["owner_public_key"],
      "provider" => provider,
      "provider_public_key" => provider["public_key"],
      "capability" => capability,
      "capability_id" => capability_id,
      "package_version" => verified["package_version"],
      "package_hash" => verified["package_hash"],
      "release_version" => get_in(verified, ["release", "version"]),
      "channel" => get_in(verified, ["release", "channel"]),
      "published_at" => verified["published_at"],
      "signer_public_key" => get_in(verified, ["signature", "signer_public_key"]),
      "signature" => verified["signature"],
      "usage" => usage_from_capability(command, capability),
      "installed_at" => System.system_time(:millisecond),
      "pinned" => Keyword.get(opts, :pinned, false),
      "trust_state_at_install" => Keyword.get(opts, :trust_state_at_install, "allowed")
    }
  end

  defp validate_install_target(document, verified, command, opts) do
    cond do
      reserved_command?(command) ->
        {:error, :reserved_command}

      existing = Enum.find(Map.get(document, "tools", []), &(&1["command"] == command)) ->
        cond do
          Keyword.get(opts, :replace, true) == false ->
            {:error, :command_conflict}

          existing["signer_public_key"] != nil and
              existing["signer_public_key"] != get_in(verified, ["signature", "signer_public_key"]) ->
            {:error, :signer_conflict}

          true ->
            :ok
        end

      true ->
        :ok
    end
  end

  defp upsert_tool(tools, install) do
    tools
    |> Enum.reject(&(&1["command"] == install["command"]))
    |> Kernel.++([install])
    |> Enum.sort_by(& &1["command"])
  end

  defp cli_command(capability) when is_map(capability) do
    case InterfaceManifest.cli(capability) do
      %{"namespace" => value} when is_binary(value) and value != "" -> value
      _ -> nil
    end
  end

  defp usage_suffix([]), do: ""
  defp usage_suffix(args), do: " " <> Enum.map_join(args, " ", &usage_token/1)

  defp normalize_command(value) when is_binary(value) do
    value
    |> String.downcase()
    |> String.replace(~r/[^a-z0-9-]+/u, "-")
    |> String.replace(~r/-+/u, "-")
    |> String.trim("-")
  end

  defp owner_document(owner) do
    with {:ok, owner_pk} <- owner_public_key(owner) do
      {:ok,
       %{
         "owner_name" => Identity.name(owner_pk),
         "owner_public_key" => Base.encode16(owner_pk, case: :lower)
       }}
    end
  end

  defp owner_public_key(%Identity{public_key: owner_pk}), do: {:ok, owner_pk}
  defp owner_public_key(<<owner_pk::binary-size(32)>>), do: {:ok, owner_pk}
  defp owner_public_key(_owner), do: {:error, :invalid_owner}

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
          with {:ok, owner_document} <- owner_document(owner) do
            {:ok, owner_document |> Map.put("version", @registry_version) |> Map.put("tools", [])}
          end

        {:error, reason} ->
          {:error, reason}
      end
    end
  end

  defp persist_document(owner, document, opts) do
    with {:ok, dir} <- registry_dir(owner, opts),
         :ok <- File.mkdir_p(dir),
         {:ok, path} <- owner_path(owner, opts) do
      body = document |> :json.encode() |> IO.iodata_to_binary()
      File.write(path, body)
    end
  end

  defp registry_root(opts) do
    opts
    |> Keyword.get(:dir, Application.get_env(:arc_cli, :tool_registry_dir, @default_dir))
    |> Path.expand()
  end

  defp registry_dir(owner, opts) do
    with {:ok, owner_pk} <- owner_public_key(owner) do
      {:ok, Path.join(registry_root(opts), Base.encode16(owner_pk, case: :lower))}
    end
  end

  defp owner_path(owner, opts) do
    with {:ok, dir} <- registry_dir(owner, opts) do
      {:ok, Path.join(dir, "tools.json")}
    end
  end
end
