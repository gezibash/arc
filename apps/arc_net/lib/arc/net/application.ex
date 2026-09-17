defmodule Arc.Net.Application do
  # See https://hexdocs.pm/elixir/Application.html
  # for more information on OTP Applications
  @moduledoc false

  use Application

  @impl true
  def start(_type, _args) do
    base_children = [
      {Task.Supervisor, name: Arc.Net.TaskSupervisor},
      {Registry, keys: :unique, name: Arc.Net.TransportRegistry},
      {Registry, keys: :unique, name: Arc.Net.DirectRegistry},
      {DynamicSupervisor, strategy: :one_for_one, name: Arc.Net.TransportSupervisor},
      Arc.Net.TransportManager
    ]

    with {:ok, relay_children} <- managed_relay_children() do
      children = base_children ++ relay_children

      # See https://hexdocs.pm/elixir/Supervisor.html
      # for more information on supervision strategies.
      Supervisor.start_link(children, strategy: :one_for_one, name: Arc.Net.Supervisor)
    end
  end

  defp managed_relay_children do
    case Application.get_env(:arc_net, :managed_relay) do
      nil ->
        {:ok, []}

      document ->
        with %{"role" => "relay", "key" => key, "port" => port} <- document,
             true <- is_binary(key) and byte_size(key) > 0,
             true <- is_integer(port) and port > 0 and port < 65_536,
             peers when is_list(peers) <- Map.get(document, "peers", []),
             transit when is_boolean(transit) <- Map.get(document, "transit", false),
             {:ok, identity} <- Arc.Identity.KeyStore.get(key),
             {:ok, relay_peers} <- normalize_peers(peers, identity.public_key) do
          relay_opts =
            [
              relay_identity: identity,
              federation_peers: relay_peers,
              federation_transit: transit
            ]

          {:ok,
           [
             %{
               id: Arc.Net.Relay,
               start: {Arc.Net.Relay, :start_link, [port, relay_opts]},
               type: :worker
             }
           ]}
        else
          _ -> {:error, {:managed_relay, :invalid_configuration}}
        end
    end
  end

  defp normalize_peers(peers, own_key) when length(peers) <= 16 do
    with true <- Enum.all?(peers, &is_binary/1),
         {:ok, parsed} <- parse_peers(peers, own_key),
         true <- MapSet.size(MapSet.new(parsed, & &1.public_key)) == length(parsed) do
      {:ok, parsed}
    else
      _ -> {:error, :invalid_peers}
    end
  end

  defp normalize_peers(_peers, _own_key), do: {:error, :invalid_peers}

  defp parse_peers(peers, own_key) do
    Enum.reduce_while(peers, {:ok, []}, fn peer, {:ok, acc} ->
      case parse_peer(peer, own_key) do
        {:ok, parsed} -> {:cont, {:ok, [parsed | acc]}}
        {:error, _} = error -> {:halt, error}
      end
    end)
    |> then(fn
      {:ok, parsed} -> {:ok, Enum.reverse(parsed)}
      error -> error
    end)
  end

  defp parse_peer(peer, own_key) do
    with [key, address] <- String.split(peer, "@", parts: 2),
         {:ok, <<public_key::binary-size(32)>>} <- Base.decode16(key, case: :mixed),
         {host, port} <- Arc.Net.relay_address_from(address),
         false <- public_key == own_key do
      {:ok, %{public_key: public_key, host: host, port: port}}
    else
      _ -> {:error, :invalid_peer}
    end
  end
end
