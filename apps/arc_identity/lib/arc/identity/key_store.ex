defmodule Arc.Identity.KeyStore do
  @moduledoc """
  Multi-key store in ~/.config/arc/keys/.

  Each identity is stored as a TOML file named by its petname.
  A default key can be set and is stored in ~/.config/arc/default_key.

  Per-terminal key selection is done via the ARC_KEY env var,
  which takes a petname or unambiguous prefix.
  """

  alias Arc.Identity

  @keys_dir Path.join(["~", ".config", "arc", "keys"])
  @default_file Path.join(["~", ".config", "arc", "default_key"])

  @doc """
  Generate a new identity and store it.
  """
  @spec generate() :: {:ok, Identity.t()}
  def generate do
    identity = Identity.generate()
    :ok = save(identity)
    {:ok, identity}
  end

  @doc """
  Save an identity to the key store.
  """
  @spec save(Identity.t()) :: :ok | {:error, term()}
  def save(%Identity{} = identity) do
    dir = keys_dir()
    File.mkdir_p!(dir)

    name = Identity.name(identity)
    path = Path.join(dir, "#{name}.toml")
    seed_hex = Base.encode16(identity.seed, case: :lower)

    content = """
    [identity]
    seed = "#{seed_hex}"
    """

    File.write(path, content)
  end

  @doc """
  List all stored identities. Returns list of {petname, identity}.
  """
  @spec list() :: [{String.t(), Identity.t()}]
  def list do
    dir = keys_dir()

    case File.ls(dir) do
      {:ok, files} ->
        files
        |> Enum.filter(&String.ends_with?(&1, ".toml"))
        |> Enum.sort()
        |> Enum.flat_map(fn file ->
          path = Path.join(dir, file)

          case load_file(path) do
            {:ok, identity} -> [{Identity.name(identity), identity}]
            _ -> []
          end
        end)

      {:error, :enoent} ->
        []
    end
  end

  @doc """
  Get an identity by petname or unambiguous prefix.
  """
  @spec get(String.t()) :: {:ok, Identity.t()} | {:error, :not_found | :ambiguous}
  def get(name_or_prefix) do
    keys = list()

    case Enum.filter(keys, fn {name, _} -> name == name_or_prefix end) do
      [{_, identity}] ->
        {:ok, identity}

      [] ->
        # Try prefix match
        matches =
          Enum.filter(keys, fn {name, _} -> String.starts_with?(name, name_or_prefix) end)

        case matches do
          [{_, identity}] -> {:ok, identity}
          [] -> {:error, :not_found}
          _ -> {:error, :ambiguous}
        end

      _ ->
        {:error, :ambiguous}
    end
  end

  @doc """
  Get an identity by its exact public key.
  """
  @spec get_by_public_key(Identity.public_key()) :: {:ok, Identity.t()} | {:error, :not_found}
  def get_by_public_key(<<public_key::binary-size(32)>>) do
    case Enum.find(list(), fn {_name, identity} -> identity.public_key == public_key end) do
      {_name, identity} -> {:ok, identity}
      nil -> {:error, :not_found}
    end
  end

  @doc """
  Remove a key by petname or prefix.
  """
  @spec remove(String.t()) :: :ok | {:error, :not_found | :ambiguous}
  def remove(name_or_prefix) do
    case get(name_or_prefix) do
      {:ok, identity} ->
        name = Identity.name(identity)
        path = Path.join(keys_dir(), "#{name}.toml")
        File.rm(path)

        # Clear default if it was the removed key
        case default_name() do
          {:ok, ^name} -> File.rm(default_file())
          _ -> :ok
        end

      {:error, _} = err ->
        err
    end
  end

  @doc """
  Set the default key by petname or prefix.
  """
  @spec set_default(String.t()) :: :ok | {:error, :not_found | :ambiguous}
  def set_default(name_or_prefix) do
    case get(name_or_prefix) do
      {:ok, identity} ->
        name = Identity.name(identity)
        File.mkdir_p!(Path.dirname(default_file()))
        File.write!(default_file(), name)
        :ok

      {:error, _} = err ->
        err
    end
  end

  @doc """
  Get the default identity.
  """
  @spec default() :: {:ok, Identity.t()} | {:error, term()}
  def default do
    case default_name() do
      {:ok, name} -> get(name)
      {:error, _} = err -> err
    end
  end

  @doc """
  Get the default key name.
  """
  @spec default_name() :: {:ok, String.t()} | {:error, :no_default}
  def default_name do
    case File.read(default_file()) do
      {:ok, name} ->
        name = String.trim(name)
        if name == "", do: {:error, :no_default}, else: {:ok, name}

      {:error, :enoent} ->
        {:error, :no_default}
    end
  end

  @doc """
  Resolve the active identity. Checks ARC_KEY env, then default.
  """
  @spec resolve_active() :: {:ok, Identity.t()} | {:error, term()}
  def resolve_active do
    case System.get_env("ARC_KEY") do
      nil -> default()
      name -> get(name)
    end
  end

  defp keys_dir, do: Path.expand(Application.get_env(:arc_identity, :keys_dir, @keys_dir))

  defp default_file do
    Path.expand(Application.get_env(:arc_identity, :default_file, @default_file))
  end

  defp load_file(path) do
    with {:ok, content} <- File.read(path),
         {:ok, parsed} <- TomlElixir.decode(content),
         %{"identity" => %{"seed" => hex}} <- parsed,
         {:ok, <<seed::binary-size(32)>>} <- Base.decode16(hex, case: :mixed) do
      {:ok, Identity.from_seed(seed)}
    else
      _ -> {:error, :invalid_keyring}
    end
  end
end
