defmodule Arc.CLI.Application do
  @moduledoc false

  use Application

  @impl true
  def start(_type, _args) do
    case Application.get_env(:arc_cli, :service_config) do
      nil ->
        Supervisor.start_link([], strategy: :one_for_one, name: __MODULE__.Supervisor)

      document ->
        case normalize_service_config(document) do
          {:ok, config} ->
            manager = Module.concat([Arc, CLI, Update, Manager])

            children = [
              %{
                id: manager,
                start: {manager, :start_link, [[config: config]]},
                type: :worker
              },
              {Arc.CLI.Update.Admin, state_dir: config.update.state_dir, manager: manager}
            ]

            Supervisor.start_link(children, strategy: :one_for_one, name: __MODULE__.Supervisor)

          {:error, reason} ->
            {:error, {:invalid_service_config, reason}}
        end
    end
  end

  @doc false
  def normalize_service_config(document) when is_map(document) do
    with :ok <- only_keys(document, ["role", "key", "port", "peers", "transit", "update"]),
         "relay" <- Map.get(document, "role"),
         key when is_binary(key) and byte_size(key) > 0 <- Map.get(document, "key"),
         port when is_integer(port) and port > 0 and port < 65_536 <- Map.get(document, "port"),
         peers when is_list(peers) <- Map.get(document, "peers", []),
         true <- Enum.all?(peers, &is_binary/1),
         transit when is_boolean(transit) <- Map.get(document, "transit", false),
         {:ok, update} <- normalize_update(Map.get(document, "update")) do
      {:ok, %{role: :relay, key: key, port: port, peers: peers, transit: transit, update: update}}
    else
      :error -> {:error, :unknown_field}
      _ -> {:error, :invalid_service}
    end
  end

  def normalize_service_config(_), do: {:error, :invalid_service}

  defp normalize_update(update) when is_map(update) do
    with :ok <- only_keys(update, ["state_dir", "publisher", "channel", "pin", "source"]),
         state_dir when is_binary(state_dir) <- Map.get(update, "state_dir"),
         true <- Path.type(state_dir) == :absolute,
         publisher when is_binary(publisher) <- Map.get(update, "publisher"),
         {:ok, <<_::binary-size(32)>>} <- Base.decode16(publisher, case: :mixed),
         channel when channel in ["stable", "beta"] <- Map.get(update, "channel", "stable"),
         {:ok, pin} <- normalize_pin(Map.get(update, "pin")),
         {:ok, source} <- normalize_source(Map.get(update, "source")) do
      {:ok,
       %{
         state_dir: Path.expand(state_dir),
         publisher: String.downcase(publisher),
         channel: channel,
         pin: pin,
         source: source
       }}
    else
      :error -> {:error, :unknown_update_field}
      _ -> {:error, :invalid_update}
    end
  end

  defp normalize_update(_), do: {:error, :invalid_update}

  defp normalize_pin(nil), do: {:ok, nil}
  defp normalize_pin(:null), do: {:ok, nil}
  defp normalize_pin(pin) when is_binary(pin) and byte_size(pin) > 0, do: {:ok, pin}
  defp normalize_pin(_), do: {:error, :invalid_pin}

  defp normalize_source(
         %{"uri" => uri, "relay" => relay, "relay_pubkey" => relay_pubkey} = source
       )
       when is_binary(uri) and is_binary(relay) and is_binary(relay_pubkey) do
    with :ok <- only_keys(source, ["uri", "relay", "relay_pubkey"]),
         {:ok, %{scheme: "releases"}} <- Arc.Data.Protocol.parse(uri),
         {_host, _port} <- Arc.Net.relay_address_from(relay),
         {:ok, <<_::binary-size(32)>>} <- Base.decode16(relay_pubkey, case: :mixed) do
      {:ok, %{uri: uri, relay: relay, relay_pubkey: String.downcase(relay_pubkey)}}
    else
      _ -> {:error, :invalid_source}
    end
  end

  defp normalize_source(%{"local_dir" => directory} = source) when is_binary(directory) do
    with :ok <- only_keys(source, ["local_dir"]),
         true <- Path.type(directory) == :absolute do
      {:ok, %{local_dir: Path.expand(directory)}}
    else
      _ -> {:error, :invalid_source}
    end
  end

  defp normalize_source(_), do: {:error, :invalid_source}

  defp only_keys(map, allowed) do
    if Enum.all?(Map.keys(map), &(&1 in allowed)), do: :ok, else: :error
  end
end
