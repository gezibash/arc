defmodule Arc.MCP.DynamicToolRegistry do
  @moduledoc """
  Identity- and task-scoped mount registry for ARC capability working sets.

  The registry keeps a bounded set of mounted capabilities per owner identity
  and task so different local ARC keys do not share the same toolbox.
  """

  alias Arc.Identity

  @default_dir Path.join(["~", ".config", "arc", "mounts"])
  @default_limit 10

  @type owner_scope :: Identity.t() | Identity.public_key()

  @spec list(owner_scope(), String.t(), keyword()) :: {:ok, [map()]} | {:error, term()}
  def list(owner, task, opts \\ []) when is_binary(task) and task != "" do
    with {:ok, document} <- load_document(owner, task, opts) do
      {:ok, Map.get(document, "mounts", [])}
    end
  end

  @spec get(owner_scope(), String.t(), String.t(), String.t(), keyword()) ::
          {:ok, map()} | {:error, term()}
  def get(owner, task, provider_name, capability_id, opts \\ [])
      when is_binary(task) and task != "" and is_binary(provider_name) and
             is_binary(capability_id) do
    with {:ok, mounts} <- list(owner, task, opts) do
      mount_id = provider_name <> "/" <> capability_id

      case Enum.find(mounts, &(&1["mount_id"] == mount_id)) do
        nil -> {:error, :not_found}
        mount -> {:ok, mount}
      end
    end
  end

  @spec mount(owner_scope(), String.t(), map(), keyword()) :: {:ok, map()} | {:error, term()}
  def mount(
        owner,
        task,
        %{"provider" => provider, "capability" => capability} = detail,
        opts \\ []
      )
      when is_binary(task) and task != "" and is_map(provider) and is_map(capability) do
    with {:ok, document} <- load_document(owner, task, opts),
         {:ok, owner_document} <- owner_document(owner) do
      mounts = Map.get(document, "mounts", [])
      mount = normalize_mount(owner_document, task, detail)
      mount_id = mount["mount_id"]
      existing = Enum.find(mounts, &(&1["mount_id"] == mount_id))
      remaining = Enum.reject(mounts, &(&1["mount_id"] == mount_id))
      limit = mount_limit(opts)

      if existing == nil and length(remaining) >= limit do
        {:error, {:mount_limit_exceeded, limit}}
      else
        mounts = [mount | remaining] |> Enum.sort_by(& &1["mount_id"])

        document =
          owner_document
          |> Map.put("version", 1)
          |> Map.put("task", task)
          |> Map.put("mounts", mounts)

        with :ok <- persist_document(owner, task, document, opts) do
          {:ok, mount}
        end
      end
    end
  end

  @spec unmount(owner_scope(), String.t(), String.t(), String.t(), keyword()) ::
          :ok | {:error, term()}
  def unmount(owner, task, provider_name, capability_id, opts \\ [])
      when is_binary(task) and task != "" and is_binary(provider_name) and
             is_binary(capability_id) do
    with {:ok, document} <- load_document(owner, task, opts) do
      mount_id = provider_name <> "/" <> capability_id
      mounts = document |> Map.get("mounts", []) |> Enum.reject(&(&1["mount_id"] == mount_id))

      persist_document(owner, task, %{document | "mounts" => mounts}, opts)
    end
  end

  defp normalize_mount(owner_document, task, %{"provider" => provider, "capability" => capability}) do
    provider_name = provider["name"] || provider["short_name"] || "unknown"
    capability_id = capability["id"] || "unknown"

    %{
      "mount_id" => provider_name <> "/" <> capability_id,
      "owner_name" => owner_document["owner_name"],
      "owner_public_key" => owner_document["owner_public_key"],
      "task" => task,
      "provider" => provider,
      "capability" => capability,
      "mounted_at" => System.system_time(:millisecond)
    }
  end

  defp load_document(owner, task, opts) do
    with {:ok, path} <- task_path(owner, task, opts) do
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
            {:ok,
             owner_document
             |> Map.put("version", 1)
             |> Map.put("task", task)
             |> Map.put("mounts", [])}
          end

        {:error, reason} ->
          {:error, reason}
      end
    end
  end

  defp persist_document(owner, task, document, opts) do
    with {:ok, dir} <- owner_dir(owner, opts),
         :ok <- File.mkdir_p(dir),
         {:ok, path} <- task_path(owner, task, opts) do
      body = document |> :json.encode() |> IO.iodata_to_binary()
      File.write(path, body)
    end
  end

  defp mount_limit(opts) do
    Keyword.get(
      opts,
      :limit,
      Application.get_env(:arc_mcp, :dynamic_tool_registry_limit, @default_limit)
    )
  end

  defp registry_dir(opts) do
    opts
    |> Keyword.get(:dir, Application.get_env(:arc_mcp, :dynamic_tool_registry_dir, @default_dir))
    |> Path.expand()
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

  defp owner_dir(owner, opts) do
    with {:ok, owner_pk} <- owner_public_key(owner) do
      {:ok, Path.join(registry_dir(opts), Base.encode16(owner_pk, case: :lower))}
    end
  end

  defp task_path(owner, task, opts) do
    with {:ok, dir} <- owner_dir(owner, opts) do
      {:ok, Path.join(dir, task <> ".json")}
    end
  end
end
