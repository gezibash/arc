defmodule Arc.Net.RelayConfig do
  @moduledoc """
  The local, pinned relay selection for ARC clients.

  This module deliberately stores only public relay addresses and public-key
  pins. A changed pin is a trust failure, rather than an automatic update.
  """

  import Bitwise

  @default_port 7331
  @max_file_bytes 16_384
  @config_name "relays.json"

  @type document :: %{
          required(String.t()) => 1 | nil | String.t() | %{required(String.t()) => String.t()}
        }

  @spec path() :: String.t()
  def path do
    Application.get_env(:arc_net, :relay_config_path) ||
      Path.join([System.user_home!(), ".config", "arc", @config_name])
  end

  @spec load() :: {:ok, document()} | {:error, :invalid_relay_config}
  def load do
    case File.lstat(path()) do
      {:error, :enoent} ->
        {:ok, empty_document()}

      {:ok, %{type: :regular, size: size, mode: mode}} when size <= @max_file_bytes ->
        if private_mode?(mode), do: read_document(path()), else: invalid_config()

      _ ->
        invalid_config()
    end
  end

  @spec normalize_address(String.t()) :: {:ok, String.t()} | {:error, :invalid_relay_address}
  def normalize_address(address) when is_binary(address) do
    with true <- String.valid?(address),
         true <- address != "" and no_control_or_space?(address),
         true <- no_uri_syntax?(address),
         {:ok, host, port} <- split_address(address),
         {:ok, canonical_host} <- canonical_host(host),
         {:ok, canonical_port} <- canonical_port(port) do
      {:ok, canonical_host <> ":" <> Integer.to_string(canonical_port)}
    else
      _ -> {:error, :invalid_relay_address}
    end
  end

  def normalize_address(_), do: {:error, :invalid_relay_address}

  @spec remember(String.t(), String.t()) ::
          :ok | {:error, :relay_pubkey_mismatch | :invalid_relay_config}
  def remember(address, pin_hex) do
    with {:ok, canonical_address} <- normalize_address(address),
         {:ok, canonical_pin} <- normalize_pin(pin_hex),
         :ok <- with_lock(path(), fn -> remember_locked(canonical_address, canonical_pin) end) do
      :ok
    else
      {:error, :relay_pubkey_mismatch} = error -> error
      _ -> invalid_config()
    end
  end

  @spec resolve(keyword()) ::
          {:ok, %{address: String.t() | nil, public_key: String.t() | nil}}
          | {:error, :invalid_relay_config}
  def resolve(opts \\ [])

  def resolve(opts) when is_list(opts) do
    relay_opt = Keyword.get(opts, :relay)
    pin_opt = Keyword.get(opts, :relay_pubkey)

    if not is_nil(relay_opt) and not is_nil(pin_opt) do
      explicit_resolution(relay_opt, pin_opt)
    else
      resolve_with_config(relay_opt, pin_opt)
    end
  end

  def resolve(_), do: invalid_config()

  defp explicit_resolution(address, pin) do
    with {:ok, canonical_address} <- normalize_address(address),
         {:ok, canonical_pin} <- normalize_pin(pin) do
      {:ok, %{address: canonical_address, public_key: canonical_pin}}
    else
      _ -> invalid_config()
    end
  end

  defp resolve_with_config(relay_opt, pin_opt) do
    with {:ok, document} <- load(),
         {:ok, address, address_source} <- choose_address(relay_opt, document),
         {:ok, pin} <- choose_pin(pin_opt, address, address_source, document) do
      {:ok, %{address: address, public_key: pin}}
    else
      _ -> invalid_config()
    end
  end

  defp choose_address(relay_opt, _document) when not is_nil(relay_opt) do
    case normalize_address(relay_opt) do
      {:ok, address} -> {:ok, address, :option}
      _ -> invalid_config()
    end
  end

  defp choose_address(nil, document) do
    case System.get_env("ARC_RELAY") do
      nil ->
        {:ok, document["default"], :saved}

      address ->
        case normalize_address(address) do
          {:ok, canonical_address} -> {:ok, canonical_address, :environment}
          _ -> invalid_config()
        end
    end
  end

  defp choose_pin(pin_opt, _address, _address_source, _document) when not is_nil(pin_opt) do
    case normalize_pin(pin_opt) do
      {:ok, pin} -> {:ok, pin}
      _ -> invalid_config()
    end
  end

  defp choose_pin(nil, address, _address_source, document) do
    case System.get_env("ARC_RELAY_PUBKEY") do
      nil ->
        {:ok, saved_pin(document, address)}

      pin ->
        case normalize_pin(pin) do
          {:ok, canonical_pin} -> {:ok, canonical_pin}
          _ -> invalid_config()
        end
    end
  end

  defp saved_pin(_document, nil), do: nil

  defp saved_pin(%{"relays" => relays}, address), do: Map.get(relays, address)

  defp remember_locked(canonical_address, canonical_pin) do
    with {:ok, document} <- load(),
         :ok <- pin_matches?(document["relays"][canonical_address], canonical_pin) do
      write_document(%{
        "version" => 1,
        "default" => canonical_address,
        "relays" => Map.put(document["relays"], canonical_address, canonical_pin)
      })
    end
  end

  defp pin_matches?(nil, _pin), do: :ok
  defp pin_matches?(pin, pin), do: :ok
  defp pin_matches?(_existing, _new), do: {:error, :relay_pubkey_mismatch}

  defp write_document(document) do
    target = path()
    directory = Path.dirname(target)
    temporary = target <> "." <> Base.encode16(:crypto.strong_rand_bytes(12), case: :lower)
    bytes = document |> :json.encode() |> IO.iodata_to_binary()

    cond do
      byte_size(bytes) > @max_file_bytes ->
        invalid_config()

      File.mkdir_p(directory) != :ok ->
        invalid_config()

      true ->
        case File.open(temporary, [:write, :binary, :exclusive]) do
          {:ok, file} ->
            temporary_result(
              write_temporary(file, temporary, bytes),
              temporary,
              target,
              directory
            )

          _ ->
            invalid_config()
        end
    end
  end

  defp write_temporary(file, temporary, bytes) do
    result =
      with :ok <- File.chmod(temporary, 0o600),
           :ok <- IO.binwrite(file, bytes) do
        :file.sync(file)
      end

    _ = File.close(file)
    result
  end

  defp temporary_result(:ok, temporary, target, directory) do
    case File.rename(temporary, target) do
      :ok -> sync_directory(directory)
      error -> error
    end
  end

  defp temporary_result(error, temporary, _target, _directory) do
    _ = File.rm(temporary)
    error
  end

  defp with_lock(target, fun) when is_function(fun, 0) do
    lock = target <> ".lock"

    with :ok <- File.mkdir_p(Path.dirname(target)),
         {:ok, file} <- File.open(lock, [:write, :binary, :exclusive]) do
      try do
        with :ok <- File.chmod(lock, 0o600), do: fun.()
      after
        File.close(file)
        _ = File.rm(lock)
      end
    else
      _ -> invalid_config()
    end
  end

  defp read_document(config_path) do
    with {:ok, bytes} <- File.read(config_path),
         document when is_map(document) <- :json.decode(bytes),
         {:ok, canonical_document} <- validate_document(document) do
      {:ok, canonical_document}
    else
      _ -> invalid_config()
    end
  rescue
    _ -> invalid_config()
  end

  defp validate_document(%{"version" => 1, "default" => default, "relays" => relays} = document)
       when map_size(document) == 3 and is_map(relays) do
    default = null_to_nil(default)

    with {:ok, canonical_relays} <- validate_relays(relays),
         :ok <- validate_default(default, canonical_relays) do
      {:ok, %{"version" => 1, "default" => default, "relays" => canonical_relays}}
    end
  end

  defp validate_document(_), do: invalid_config()

  defp validate_relays(relays) do
    Enum.reduce_while(relays, {:ok, %{}}, fn
      {address, pin}, {:ok, acc} ->
        with {:ok, ^address} <- normalize_address(address),
             {:ok, ^pin} <- normalize_pin(pin) do
          {:cont, {:ok, Map.put(acc, address, pin)}}
        else
          _ -> {:halt, invalid_config()}
        end

      _, _ ->
        {:halt, invalid_config()}
    end)
  end

  defp validate_default(nil, _relays), do: :ok

  defp validate_default(default, relays) when is_binary(default) and is_map_key(relays, default),
    do: :ok

  defp validate_default(_, _), do: invalid_config()

  defp split_address("[" <> rest) do
    case String.split(rest, "]", parts: 2) do
      [host, ""] -> {:ok, "[" <> host <> "]", @default_port}
      [host, ":" <> port] -> {:ok, "[" <> host <> "]", port}
      _ -> {:error, :invalid_relay_address}
    end
  end

  defp split_address(address) do
    case String.split(address, ":", parts: 2) do
      [host] -> {:ok, host, @default_port}
      [host, port] -> {:ok, host, port}
      _ -> {:error, :invalid_relay_address}
    end
  end

  defp canonical_host("[" <> rest) do
    case String.ends_with?(rest, "]") do
      true ->
        host = String.slice(rest, 0, byte_size(rest) - 1)

        if String.contains?(host, ["[", "]"]) do
          {:error, :invalid_relay_address}
        else
          case :inet.parse_address(String.to_charlist(host)) do
            {:ok, ip} when tuple_size(ip) == 8 ->
              canonical = ip |> :inet.ntoa() |> to_string() |> String.downcase()
              {:ok, "[" <> canonical <> "]"}

            _ ->
              {:error, :invalid_relay_address}
          end
        end

      false ->
        {:error, :invalid_relay_address}
    end
  end

  defp canonical_host(host) do
    canonical = String.downcase(host)

    cond do
      canonical == "" or String.contains?(canonical, ["[", "]", ":"]) ->
        {:error, :invalid_relay_address}

      valid_ipv4?(canonical) ->
        {:ok, canonical}

      valid_hostname?(canonical) ->
        {:ok, canonical}

      true ->
        {:error, :invalid_relay_address}
    end
  end

  defp canonical_port(port) when is_integer(port) and port in 1..65_535, do: {:ok, port}

  defp canonical_port(port) when is_binary(port) do
    case Integer.parse(port) do
      {number, ""} when number in 1..65_535 -> {:ok, number}
      _ -> {:error, :invalid_relay_address}
    end
  end

  defp canonical_port(_), do: {:error, :invalid_relay_address}

  defp normalize_pin(pin) when is_binary(pin) do
    case Arc.Net.relay_pubkey_from(pin) do
      public_key when is_binary(public_key) and byte_size(public_key) == 32 ->
        {:ok, Base.encode16(public_key, case: :lower)}

      _ ->
        invalid_config()
    end
  end

  defp normalize_pin(_), do: invalid_config()

  defp valid_ipv4?(host) do
    case :inet.parse_address(String.to_charlist(host)) do
      {:ok, ip} when tuple_size(ip) == 4 -> true
      _ -> false
    end
  end

  defp valid_hostname?(host) do
    byte_size(host) <= 253 and
      Regex.match?(
        ~r/\A(?=.{1,253}\z)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\z/,
        host
      )
  end

  defp no_control_or_space?(address), do: not Regex.match?(~r/[\s\x00-\x1F\x7F]/, address)
  defp no_uri_syntax?(address), do: not String.contains?(address, ["/", "?", "#", "@"])
  defp private_mode?(mode), do: (mode &&& 0o077) == 0
  defp null_to_nil(:null), do: nil
  defp null_to_nil(value), do: value
  defp empty_document, do: %{"version" => 1, "default" => nil, "relays" => %{}}
  defp invalid_config, do: {:error, :invalid_relay_config}

  defp sync_directory(directory) do
    with {:ok, handle} <- :file.open(String.to_charlist(directory), [:read, :raw, :directory]) do
      result = :file.sync(handle)
      closed = :file.close(handle)
      if result == :ok, do: closed, else: result
    end
  end
end
