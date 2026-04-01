defmodule Arc.CLI.TrustStore do
  @moduledoc """
  Owner-scoped trust decisions for remote capability signers.
  """

  alias Arc.Identity

  @default_dir Path.join(["~", ".config", "arc", "trust"])

  @type owner_scope :: Identity.t() | Identity.public_key()
  @spec list(owner_scope(), keyword()) :: {:ok, [map()]} | {:error, term()}
  def list(owner, opts \\ []) do
    with {:ok, document} <- load_document(owner, opts) do
      {:ok, Map.get(document, "signers", [])}
    end
  end

  @spec get(owner_scope(), String.t(), keyword()) :: {:ok, map()} | {:error, term()}
  def get(owner, signer_public_key, opts \\ [])
      when is_binary(signer_public_key) and signer_public_key != "" do
    with {:ok, signers} <- list(owner, opts) do
      case Enum.find(signers, &(&1["signer_public_key"] == normalize_hex(signer_public_key))) do
        nil -> {:error, :not_found}
        signer -> {:ok, signer}
      end
    end
  end

  @spec allow(owner_scope(), String.t(), keyword()) :: {:ok, map()} | {:error, term()}
  def allow(owner, signer_public_key, opts \\ []) do
    put(owner, signer_public_key, "allowed", opts)
  end

  @spec deny(owner_scope(), String.t(), keyword()) :: {:ok, map()} | {:error, term()}
  def deny(owner, signer_public_key, opts \\ []) do
    put(owner, signer_public_key, "denied", opts)
  end

  defp put(owner, signer_public_key, state, opts) when state in ["allowed", "denied"] do
    with {:ok, document} <- load_document(owner, opts),
         {:ok, owner_document} <- owner_document(owner) do
      signer_public_key = normalize_hex(signer_public_key)

      record =
        %{
          "signer_public_key" => signer_public_key,
          "state" => state,
          "scope" => "global",
          "first_trusted_at" => System.system_time(:millisecond)
        }
        |> maybe_put("note", normalize_note(Keyword.get(opts, :note)))
        |> merge_existing_timestamp(existing(document, signer_public_key))

      signers =
        document
        |> Map.get("signers", [])
        |> Enum.reject(&(&1["signer_public_key"] == signer_public_key))
        |> Kernel.++([record])
        |> Enum.sort_by(& &1["signer_public_key"])

      document = owner_document |> Map.put("version", 1) |> Map.put("signers", signers)

      with :ok <- persist_document(owner, document, opts) do
        {:ok, record}
      end
    end
  end

  defp existing(document, signer_public_key) do
    Enum.find(Map.get(document, "signers", []), &(&1["signer_public_key"] == signer_public_key))
  end

  defp merge_existing_timestamp(record, %{"first_trusted_at" => first_trusted_at}) do
    Map.put(record, "first_trusted_at", first_trusted_at)
  end

  defp merge_existing_timestamp(record, _existing), do: record

  defp normalize_note(note) when is_binary(note) and note != "", do: note
  defp normalize_note(_note), do: nil

  defp normalize_hex(value) do
    value
    |> String.trim()
    |> String.downcase()
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

  defp load_document(owner, opts) do
    with {:ok, path} <- owner_path(owner, opts) do
      case File.read(path) do
        {:ok, content} ->
          try do
            case :json.decode(content) do
              %{} = document -> {:ok, document}
              _ -> {:error, :invalid_trust_store}
            end
          rescue
            _ -> {:error, :invalid_trust_store}
          end

        {:error, :enoent} ->
          with {:ok, owner_document} <- owner_document(owner) do
            {:ok, owner_document |> Map.put("version", 1) |> Map.put("signers", [])}
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
    |> Keyword.get(:dir, Application.get_env(:arc_cli, :trust_store_dir, @default_dir))
    |> Path.expand()
  end

  defp registry_dir(owner, opts) do
    with {:ok, owner_pk} <- owner_public_key(owner) do
      {:ok, Path.join(registry_root(opts), Base.encode16(owner_pk, case: :lower))}
    end
  end

  defp owner_path(owner, opts) do
    with {:ok, dir} <- registry_dir(owner, opts) do
      {:ok, Path.join(dir, "trust.json")}
    end
  end

  defp maybe_put(map, _key, nil), do: map
  defp maybe_put(map, key, value), do: Map.put(map, key, value)
end
