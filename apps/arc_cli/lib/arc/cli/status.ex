defmodule Arc.CLI.Status do
  @moduledoc """
  Read-only overview of client settings and the local ARC host's live connections.
  """

  alias Arc.CLI.Keys
  alias Arc.CLI.StatusDocker
  alias Arc.CLI.StatusProbe
  alias Arc.Host.Client
  alias Arc.Host.Service
  alias Arc.Identity
  alias Arc.Identity.KeyStore

  @options [
    check: :boolean,
    json: :boolean,
    socket: :string,
    relay: :string,
    relay_pubkey: :string,
    docker: :boolean,
    docker_project: :string
  ]

  def run(args) do
    case OptionParser.parse(args, strict: @options) do
      {opts, [], []} ->
        report = snapshot(opts)
        if opts[:json], do: print_json(report), else: print_report(report)
        if failed?(report), do: Arc.CLI.Exit.halt(1)

      _ ->
        IO.puts(
          :stderr,
          "usage: arc status [--check] [--json] [--socket PATH] [--relay host:port] " <>
            "[--relay-pubkey KEY] [--no-docker] [--docker-project NAME]"
        )

        Arc.CLI.Exit.halt(1)
    end
  end

  defp snapshot(opts) do
    %{
      "version" => Arc.CLI.version_string(),
      "identity" => identity_status(),
      "relay" => relay_status(opts),
      "host" => host_status(opts[:socket] || Service.default_socket_path()),
      "docker" => docker_status(opts)
    }
  end

  defp docker_status(opts) do
    if Keyword.get(opts, :docker, true) do
      StatusDocker.snapshot(Keyword.get(opts, :docker_project, "arc-local"))
    else
      %{"status" => "skipped", "services" => []}
    end
  end

  defp identity_status do
    case KeyStore.resolve_active_with_source() do
      {:ok, identity, source} ->
        %{
          "status" => "selected",
          "name" => Identity.name(identity),
          "public_key" => Identity.encode_public_key(identity),
          "source" => source_label(source)
        }

      {:error, :no_default} ->
        %{"status" => "not_selected", "message" => Keys.describe_error(:no_default)}

      {:error, reason} ->
        %{"status" => "error", "message" => Keys.describe_error(reason)}
    end
  end

  defp relay_status(opts) do
    address = Keyword.get(opts, :relay, System.get_env("ARC_RELAY"))
    pin_text = Keyword.get(opts, :relay_pubkey, System.get_env("ARC_RELAY_PUBKEY"))
    relay = Arc.Net.relay_address_from(address)
    pin = Arc.Net.relay_pubkey_from(pin_text)

    cond do
      address == nil and pin_text == nil ->
        %{"status" => "local", "message" => "No relay configured; commands use local delivery."}

      relay == nil ->
        %{"status" => "error", "message" => "Set --relay or ARC_RELAY to host:port."}

      pin_text != nil and pin == nil ->
        %{"status" => "error", "message" => "Invalid relay public key; expected hex or base64."}

      true ->
        configured_relay(opts, relay, pin)
    end
  end

  defp configured_relay(opts, {host, port} = relay, pin) do
    %{
      "status" => "configured",
      "host" => List.to_string(host),
      "port" => port,
      "source" => if(opts[:relay], do: "--relay", else: "ARC_RELAY"),
      "pinned" => pin != nil,
      "check" =>
        if(opts[:check], do: StatusProbe.check(relay, pin), else: %{"status" => "not_checked"})
    }
  end

  defp host_status(socket_path) do
    socket_path = Path.expand(socket_path)
    task = Task.async(fn -> read_host(socket_path) end)

    case Task.yield(task, 1_500) || Task.shutdown(task, :brutal_kill) do
      {:ok, result} -> result
      _ -> %{"status" => "unavailable", "socket_path" => socket_path}
    end
  end

  defp read_host(socket_path) do
    case Client.request(socket_path, "status", %{}, timeout_ms: 1_000) do
      {:ok, %{"identity_count" => count} = host} when is_integer(count) ->
        host
        |> Map.take(["started_at", "identity_count", "identities", "relay", "relay_connections"])
        |> Map.merge(%{"status" => "running", "socket_path" => socket_path})

      {:error, reason} when reason in [:enoent, :econnrefused] ->
        %{"status" => "not_running", "socket_path" => socket_path}

      _ ->
        %{"status" => "unavailable", "socket_path" => socket_path}
    end
  end

  defp failed?(report) do
    report["identity"]["status"] == "error" or report["relay"]["status"] == "error" or
      get_in(report, ["relay", "check", "status"]) in ["unreachable", "key_mismatch"] or
      report["host"]["status"] == "unavailable"
  end

  defp print_json(report), do: report |> :json.encode() |> IO.iodata_to_binary() |> IO.puts()

  defp print_report(report) do
    IO.puts("#{report["version"]}\n\nClient")
    print_identity(report["identity"])
    print_relay(report["relay"])
    print_selected_connection(report["identity"], report["host"])
    IO.puts("\nServer")
    print_host(report["host"])
    print_docker(report["docker"])

    IO.puts(
      "\nConnections shown belong to arc host. " <>
        "Standalone serve/listen/mcp and native relay processes are not inspected."
    )
  end

  defp print_identity(%{"status" => "selected"} = identity) do
    IO.puts("  Identity: #{identity["name"]}")
    IO.puts("  Selected by: #{identity["source"]}")
    IO.puts("  Public key: #{identity["public_key"]}")
  end

  defp print_identity(identity), do: IO.puts("  Identity: #{identity["message"]}")

  defp print_relay(%{"status" => "configured"} = relay) do
    IO.puts("  Relay: #{relay["host"]}:#{relay["port"]} (#{relay["source"]})")
    IO.puts("  Pin: #{if relay["pinned"], do: "configured", else: "not configured"}")
    IO.puts("  Client mode: on demand; each command opens its own connection")
    IO.puts("  Reachability: #{check_label(relay["check"])}")
  end

  defp print_relay(relay), do: IO.puts("  Relay: #{relay["message"]}")

  defp check_label(%{"status" => "not_checked"}), do: "not checked (use --check)"

  defp check_label(%{"status" => "reachable", "pin_matches" => matches}) do
    pin = if matches, do: "; configured pin matches", else: "; no pin configured"
    "reachable (ARC greeting received#{pin}; no citizen connection opened)"
  end

  defp check_label(check), do: "#{check["status"]}: #{check["reason"]}"

  defp print_host(%{"status" => "running"} = host) do
    IO.puts("  Local host: running (#{host["socket_path"]})")
    IO.puts("  Loaded identities: #{host["identity_count"]}")
    print_host_relay(host["relay"])

    case host["relay_connections"] do
      nil -> IO.puts("  Live connections: unavailable from this host version")
      [] -> IO.puts("  Live connections: none; no identities loaded")
      connections -> Enum.each(connections, &print_connection/1)
    end
  end

  defp print_host(host) do
    state = if host["status"] == "not_running", do: "not running", else: "unavailable"
    IO.puts("  Local host: #{state} (#{host["socket_path"]})")
  end

  defp print_host_relay(%{"host" => host, "port" => port}),
    do: IO.puts("  Host relay: #{host}:#{port}")

  defp print_host_relay(_), do: IO.puts("  Host relay: local only")

  defp print_connection(connection) do
    IO.puts("  #{connection["identity"]}: #{connection["status"]}")
  end

  defp print_selected_connection(%{"public_key" => key}, %{"status" => "running"} = host) do
    case Enum.find(host["relay_connections"] || [], &(&1["public_key"] == key)) do
      nil -> IO.puts("  Identity in host: no live connection reported")
      connection -> print_selected_connection(connection)
    end
  end

  defp print_selected_connection(_, _), do: :ok

  defp print_selected_connection(%{"host" => host, "port" => port} = connection) do
    IO.puts("  Identity in host: #{connection["status"]} (#{host}:#{port})")
  end

  defp print_selected_connection(connection),
    do: IO.puts("  Identity in host: #{connection["status"]}")

  defp print_docker(%{"status" => "ok", "services" => services} = docker) do
    IO.puts("  Docker project: #{docker["project"]} (current Docker context)")

    case services do
      [] -> IO.puts("  Containers: none found")
      services -> Enum.each(services, &print_container/1)
    end
  end

  defp print_docker(%{"status" => "not_installed"}), do: IO.puts("  Docker: not installed")
  defp print_docker(%{"status" => "skipped"}), do: :ok
  defp print_docker(_), do: IO.puts("  Docker: unavailable (could not inspect containers)")

  defp print_container(service) do
    health = if service["health"], do: ", #{service["health"]}", else: ""
    IO.puts("  #{service["service"]}: #{service["state"]}#{health} (#{service["image"]})")
    if service["ports"] != "", do: IO.puts("    Ports: #{service["ports"]}")
  end

  defp source_label(:environment), do: "ARC_KEY"
  defp source_label({:file, path}), do: path
end
