defmodule Arc.Identity.KeyStore do
  @moduledoc """
  Multi-key store in ~/.config/arc/keys/.

  Each identity is stored as a TOML file named by its petname.
  Active identity selection checks ARC_KEY, then arc.key in the current
  working directory, then ~/.config/arc/default.key. Each selector contains
  a petname or unambiguous name prefix, never private key material.

  The old default_key file is read only when default.key is absent.
  """

  alias Arc.Identity

  @keys_dir Path.join(["~", ".config", "arc", "keys"])
  @default_file Path.join(["~", ".config", "arc", "default.key"])

  @type selection_source :: :environment | {:file, String.t()}

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
  @spec remove(String.t()) :: :ok | {:error, term()}
  def remove(name_or_prefix) do
    case get(name_or_prefix) do
      {:ok, identity} ->
        name = Identity.name(identity)
        path = Path.join(keys_dir(), "#{name}.toml")
        was_default = default() == {:ok, identity}

        with :ok <- File.rm(path) do
          clear_removed_default(was_default)
        end

      {:error, _} = err ->
        err
    end
  end

  @doc """
  Set the default key by petname or prefix.
  """
  @spec set_default(String.t()) :: :ok | {:error, term()}
  def set_default(name_or_prefix) do
    case get(name_or_prefix) do
      {:ok, identity} ->
        name = Identity.name(identity)

        with :ok <- File.mkdir_p(Path.dirname(default_file())),
             :ok <- File.write(default_file(), name) do
          # Retire the old selector only after the new one is safely written.
          retire_legacy_default()
        end

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
  @spec default_name() :: {:ok, String.t()} | {:error, term()}
  def default_name do
    with {:ok, name, _source} <- default_selection() do
      {:ok, name}
    end
  end

  @doc """
  Resolve the active identity: ARC_KEY, current-directory arc.key, then default.
  Only an absent selector falls through; an invalid selection returns an error.
  """
  @spec resolve_active() :: {:ok, Identity.t()} | {:error, term()}
  def resolve_active do
    with {:ok, identity, _source} <- resolve_active_with_source() do
      {:ok, identity}
    end
  end

  @doc "Resolve the active identity and identify the selector that chose it."
  @spec resolve_active_with_source() ::
          {:ok, Identity.t(), selection_source()} | {:error, term()}
  def resolve_active_with_source do
    with {:ok, name, source} <- active_selection(),
         {:ok, identity} <- get(name) do
      {:ok, identity, source}
    end
  end

  defp active_selection do
    case System.get_env("ARC_KEY") do
      nil ->
        case read_selector(Path.expand("arc.key")) do
          {:error, :enoent} -> default_selection()
          result -> result
        end

      name ->
        parse_selector(name, :environment)
    end
  end

  defp default_selection do
    case read_selector(default_file()) do
      {:error, :enoent} ->
        case read_selector(legacy_default_file()) do
          {:error, :enoent} -> {:error, :no_default}
          result -> result
        end

      result ->
        result
    end
  end

  defp read_selector(path) do
    case File.read(path) do
      {:ok, contents} ->
        parse_selector(contents, {:file, path})

      {:error, :enoent} ->
        # A dangling symlink is an explicit broken choice, not a missing file.
        case File.lstat(path) do
          {:error, :enoent} -> {:error, :enoent}
          _ -> {:error, {:identity_selector_file, path, :enoent}}
        end

      {:error, reason} ->
        {:error, {:identity_selector_file, path, reason}}
    end
  end

  defp parse_selector(contents, source) do
    if String.valid?(contents) and Regex.match?(~r/\A[a-z0-9-]+\z/, String.trim(contents)) do
      {:ok, String.trim(contents), source}
    else
      {:error, {:invalid_identity_selector, source}}
    end
  end

  defp clear_removed_default(false), do: :ok

  defp clear_removed_default(true) do
    case clear_default() do
      :ok -> :ok
      {:error, reason} -> {:error, {:key_removed, reason}}
    end
  end

  defp clear_default do
    [default_file(), legacy_default_file()]
    |> Enum.uniq()
    |> Enum.reduce_while(:ok, fn path, :ok ->
      case remove_selector(path) do
        :ok -> {:cont, :ok}
        error -> {:halt, error}
      end
    end)
  end

  defp retire_legacy_default do
    result =
      if legacy_default_file() != default_file(),
        do: remove_selector(legacy_default_file()),
        else: :ok

    case result do
      :ok -> :ok
      {:error, reason} -> {:error, {:default_saved, reason}}
    end
  end

  defp remove_selector(path) do
    case File.rm(path) do
      :ok -> :ok
      {:error, :enoent} -> :ok
      {:error, reason} -> {:error, {:identity_selector_file, path, reason}}
    end
  end

  defp keys_dir, do: Path.expand(Application.get_env(:arc_identity, :keys_dir, @keys_dir))

  defp default_file do
    Path.expand(Application.get_env(:arc_identity, :default_file, @default_file))
  end

  defp legacy_default_file, do: Path.join(Path.dirname(default_file()), "default_key")

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
