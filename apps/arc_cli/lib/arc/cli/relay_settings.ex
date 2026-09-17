defmodule Arc.CLI.RelaySettings do
  @moduledoc false

  alias Arc.Net.RelayConfig

  @type settings :: %{relay: {charlist(), 0..65_535} | nil, relay_pubkey: binary() | nil}

  @spec resolve(keyword()) :: {:ok, settings()} | {:error, :invalid_relay_config}
  def resolve(opts \\ []) do
    with :ok <- compatible_explicit_address(opts),
         {:ok, %{address: address, public_key: public_key}} <- RelayConfig.resolve(opts),
         relay <- parse_address(address),
         pin <- parse_public_key(public_key),
         :ok <- valid_pair(relay, pin) do
      {:ok, %{relay: relay, relay_pubkey: pin}}
    else
      _ -> {:error, :invalid_relay_config}
    end
  end

  defp parse_address(nil), do: nil
  defp parse_address(address) when is_binary(address), do: Arc.Net.relay_address_from(address)

  defp parse_public_key(nil), do: nil

  defp parse_public_key(public_key) when is_binary(public_key),
    do: Arc.Net.relay_pubkey_from(public_key)

  defp valid_pair(nil, nil), do: :ok
  defp valid_pair({_host, _port}, nil), do: :ok
  defp valid_pair({_host, _port}, pin) when is_binary(pin), do: :ok
  defp valid_pair(_, _), do: :error

  # Joined relays are saved in canonical host:port form. Keep the established
  # command-line and environment contract: an explicitly supplied relay still
  # needs an explicit port.
  defp compatible_explicit_address(opts) do
    address = Keyword.get(opts, :relay, System.get_env("ARC_RELAY"))

    case address do
      nil -> :ok
      value when is_binary(value) -> if(Arc.Net.relay_address_from(value), do: :ok, else: :error)
      _ -> :error
    end
  end
end
