import Config

# Managed services are deliberately opt-in. The normal `arc` wrapper uses
# `eval` and never sets ARC_SERVICE_CONFIG, so it keeps the CLI process model.
case System.get_env("ARC_SERVICE_CONFIG") do
  nil ->
    :ok

  path when is_binary(path) ->
    if Path.type(path) != :absolute do
      raise "ARC_SERVICE_CONFIG must name an absolute path"
    end

    expanded = Path.expand(path)

    document =
      with {:ok, body} <- File.read(expanded),
           decoded when is_map(decoded) <- :json.decode(body) do
        decoded
      else
        {:error, reason} -> raise "cannot read ARC_SERVICE_CONFIG: #{inspect(reason)}"
        _ -> raise "ARC_SERVICE_CONFIG must contain one JSON object"
      end

    # Both applications receive the same parsed document. Arc.Net validates
    # only relay boot settings before it starts; Arc.CLI validates update
    # settings when it starts the local administrator.
    config :arc_cli, service_config: document
    config :arc_net, managed_relay: document
end
