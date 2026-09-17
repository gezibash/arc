defmodule Arc.Net.RelayStatus do
  @moduledoc """
  Fetch a relay's public, service-owned status over an authenticated ARC
  directory connection.

  Each fetch owns a temporary in-memory identity and transport. It neither
  persists a key nor shares a transport with a citizen's active relay route.
  """

  alias Arc.Identity
  alias Arc.Net.Transport

  @timeout_ms 3_000
  @public_status_keys [
    "role",
    "state",
    "version",
    "public_key",
    "uptime_seconds",
    "federation_transit"
  ]

  @type status :: %{required(String.t()) => String.t() | non_neg_integer() | boolean()}

  @spec fetch(charlist() | String.t(), pos_integer(), binary() | nil) ::
          {:ok, status()}
          | {:error,
             :timeout
             | :unsupported
             | :status_unavailable
             | :invalid_host
             | :invalid_port
             | :invalid_relay_pubkey_pin
             | :relay_pubkey_mismatch
             | term()}
  def fetch(host, port, relay_pubkey_pin \\ nil) do
    case GenServer.start(Transport, []) do
      {:ok, transport} ->
        try do
          task =
            Task.async(fn ->
              safe_fetch_with_transport(transport, host, port, relay_pubkey_pin)
            end)

          case Task.yield(task, @timeout_ms) || Task.shutdown(task, :brutal_kill) do
            {:ok, result} -> result
            nil -> {:error, :timeout}
            {:exit, _reason} -> {:error, :status_unavailable}
          end
        after
          stop_transport(transport)
        end

      {:error, _reason} ->
        {:error, :status_unavailable}
    end
  end

  defp fetch_with_transport(transport, host, port, relay_pubkey_pin) do
    with :ok <-
           Transport.connect_relay(transport, host, port, Identity.generate(), relay_pubkey_pin),
         {:ok, relay_pubkey} <- Transport.relay_public_key(transport),
         {:ok, reply} <- Transport.directory_request(transport, :status, %{}) do
      validate_reply(reply, relay_pubkey)
    else
      {:error, _reason} = error -> error
      _ -> {:error, :status_unavailable}
    end
  end

  defp safe_fetch_with_transport(transport, host, port, relay_pubkey_pin) do
    fetch_with_transport(transport, host, port, relay_pubkey_pin)
  catch
    :exit, _reason -> {:error, :status_unavailable}
  end

  defp validate_reply(%{"ok" => true, "status" => status}, relay_pubkey) when is_map(status) do
    if valid_status?(status, relay_pubkey), do: {:ok, status}, else: {:error, :status_unavailable}
  end

  defp validate_reply(%{"ok" => false, "error" => "invalid_request"}, _relay_pubkey),
    do: {:error, :unsupported}

  defp validate_reply(%{"ok" => false}, _relay_pubkey), do: {:error, :status_unavailable}
  defp validate_reply(_, _relay_pubkey), do: {:error, :status_unavailable}

  defp valid_status?(status, relay_pubkey) do
    MapSet.equal?(MapSet.new(Map.keys(status)), MapSet.new(@public_status_keys)) and
      status["role"] == "relay" and
      status["state"] == "running" and
      valid_version?(status["version"]) and
      valid_public_key?(status["public_key"], relay_pubkey) and
      is_integer(status["uptime_seconds"]) and status["uptime_seconds"] >= 0 and
      is_boolean(status["federation_transit"])
  end

  defp valid_public_key?(value, relay_pubkey) when is_binary(value) and is_binary(relay_pubkey) do
    value == Base.encode16(relay_pubkey, case: :lower)
  end

  defp valid_public_key?(_, _), do: false

  defp valid_version?(value) when is_binary(value) do
    byte_size(value) in 1..128 and Regex.match?(~r/\A[0-9A-Za-z.+_-]+\z/, value)
  end

  defp valid_version?(_), do: false

  defp stop_transport(transport) do
    if Process.alive?(transport) do
      try do
        GenServer.stop(transport, :normal, 500)
      catch
        :exit, _ -> Process.exit(transport, :kill)
      end
    end
  end
end
