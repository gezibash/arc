defmodule Arc.CLI.ProtocolRequest do
  @moduledoc """
  Send an opaque request body to an identity-addressed ARC service.
  """

  alias Arc.Data.Agent
  alias Arc.Data.Protocol
  alias Arc.Identity.KeyStore

  @switches [
    body: :string,
    input: :string,
    output: :string,
    timeout: :integer,
    capability: :string,
    relay: :string,
    relay_pubkey: :string,
    local: :boolean,
    direct_policy: :string
  ]

  def run(["--help"]), do: help()
  def run(["help"]), do: help()

  def run(args) do
    case OptionParser.parse(args, strict: @switches) do
      {opts, [address], []} -> execute(address, opts)
      _ -> error("usage: arc request <scheme+arc://provider-key/path> --body TEXT | --input PATH")
    end
  end

  defp execute(address, opts) do
    with :ok <- validate_options(opts),
         :ok <- validate_output(opts[:output]),
         {:ok, _target} <- Protocol.parse(address),
         {:ok, relay} <- relay_options(opts),
         {:ok, direct_policy} <- load_direct_policy(opts[:direct_policy], relay),
         {:ok, body} <- read_input(opts),
         {:ok, identity} <- KeyStore.resolve_active(),
         {:ok, response} <- send_request(identity, relay, direct_policy, address, body, opts),
         :ok <- write_output(response.body, opts[:output]) do
      :ok
    else
      {:error, reason} -> error(describe_error(reason))
    end
  end

  defp validate_options(opts) do
    timeout = Keyword.get(opts, :timeout, 10_000)

    cond do
      length(Keyword.keys(opts)) != length(Enum.uniq(Keyword.keys(opts))) ->
        {:error, :duplicate_option}

      Keyword.has_key?(opts, :body) == Keyword.has_key?(opts, :input) ->
        {:error, :input_required}

      timeout < 1 or timeout > 120_000 ->
        {:error, :invalid_timeout}

      Keyword.get(opts, :local, false) and (opts[:relay] || opts[:relay_pubkey]) ->
        {:error, :conflicting_delivery_options}

      Keyword.get(opts, :local, false) and opts[:direct_policy] ->
        {:error, :direct_policy_requires_relay}

      true ->
        :ok
    end
  end

  defp relay_options(opts) do
    if opts[:local] do
      {:ok, nil}
    else
      address = opts[:relay] || System.get_env("ARC_RELAY")
      pin = opts[:relay_pubkey] || System.get_env("ARC_RELAY_PUBKEY")

      with true <- is_binary(address) or {:error, :relay_required},
           {host, port} <- Arc.Net.relay_address_from(address),
           key when is_binary(key) <- Arc.Net.relay_pubkey_from(pin) do
        {:ok, {host, port, key}}
      else
        {:error, _} = error -> error
        _ -> {:error, :invalid_relay_configuration}
      end
    end
  end

  # A direct-policy file is an explicit, scoped opt-in. Loading it before the
  # agent starts means a malformed policy cannot start a provider-side runtime
  # or open any new connection.
  defp load_direct_policy(nil, _relay), do: {:ok, nil}

  defp load_direct_policy(path, nil) when is_binary(path),
    do: {:error, :direct_policy_requires_relay}

  defp load_direct_policy(path, _relay) when is_binary(path) do
    Arc.Data.Direct.Policy.load_file(path)
  end

  defp validate_output(path) when path in [nil, "-"], do: :ok

  defp validate_output(path) do
    case File.lstat(path) do
      {:ok, _} -> {:error, :output_exists}
      {:error, :enoent} -> :ok
      {:error, _} -> {:error, :invalid_output_path}
    end
  end

  defp read_input(opts) do
    case {opts[:body], opts[:input]} do
      {body, nil} when is_binary(body) -> bounded(body)
      {nil, "-"} -> read_stdin()
      {nil, path} when is_binary(path) -> read_file(path)
    end
  end

  defp read_file(path) do
    case File.open(path, [:read, :binary]) do
      {:ok, file} ->
        try do
          read_bounded(file)
        after
          File.close(file)
        end

      {:error, _} ->
        {:error, :input_read_failed}
    end
  end

  defp read_stdin do
    encoding = :io.getopts(:standard_io) |> Keyword.get(:encoding, :unicode)
    :ok = :io.setopts(:standard_io, encoding: :latin1)

    try do
      read_bounded(:stdio)
    after
      :io.setopts(:standard_io, encoding: encoding)
    end
  end

  defp read_bounded(device) do
    case IO.binread(device, Protocol.max_body_bytes() + 1) do
      :eof -> {:ok, ""}
      {:error, _} -> {:error, :input_read_failed}
      body -> bounded(body)
    end
  end

  defp bounded(body) do
    if byte_size(body) <= Protocol.max_body_bytes(),
      do: {:ok, body},
      else: {:error, :request_too_large}
  end

  defp send_request(identity, relay, direct_policy, address, body, opts) do
    agent_opts = if direct_policy, do: [direct_policy: direct_policy], else: []

    case Agent.start_link(identity, agent_opts) do
      {:ok, agent} ->
        try do
          with :ok <- initialize_agent(agent, identity, relay) do
            Protocol.request(agent, address, body,
              timeout_ms: Keyword.get(opts, :timeout, 10_000),
              capability: Keyword.get(opts, :capability, "primary")
            )
          end
        after
          if relay, do: Arc.Net.release_relay(identity.public_key, agent)
          if Process.alive?(agent), do: GenServer.stop(agent, :normal)
        end

      {:error, _} ->
        {:error, :agent_start_failed}
    end
  end

  defp initialize_agent(agent, _identity, nil), do: Agent.publish(agent)

  defp initialize_agent(agent, identity, {host, port, pin}) do
    with :ok <- Arc.Net.acquire_relay(host, port, identity, pin, owner_pid: agent),
         do: Agent.publish_relay(agent)
  end

  defp write_output(body, nil), do: write_stdout(body)
  defp write_output(body, "-"), do: write_stdout(body)

  defp write_output(body, path) do
    case File.open(path, [:write, :binary, :exclusive]) do
      {:ok, file} ->
        try do
          case IO.binwrite(file, body) do
            :ok -> :ok
            _ -> {:error, :output_write_failed}
          end
        after
          File.close(file)
        end

      {:error, _} ->
        {:error, :output_write_failed}
    end
  end

  defp write_stdout(body) do
    encoding = :io.getopts(:standard_io) |> Keyword.get(:encoding, :unicode)
    :ok = :io.setopts(:standard_io, encoding: :latin1)

    try do
      case IO.binwrite(:stdio, body) do
        :ok -> :ok
        _ -> {:error, :output_write_failed}
      end
    after
      :io.setopts(:standard_io, encoding: encoding)
    end
  end

  defp describe_error(:input_required),
    do: "choose exactly one of --body TEXT or --input PATH (- for stdin)"

  defp describe_error(:relay_required), do: "configure --relay or ARC_RELAY, or choose --local"

  defp describe_error(:direct_policy_requires_relay),
    do: "--direct-policy requires a pinned relay; use --relay and --relay-pubkey"

  defp describe_error(:invalid_relay_configuration),
    do:
      "invalid relay address or missing/invalid relay public key; use --relay and --relay-pubkey"

  defp describe_error(:invalid_protocol_uri),
    do:
      "use scheme+arc://<full provider public key>/<resource>; queries, fragments, and escaped paths are unsupported"

  defp describe_error(:outcome_unknown),
    do:
      "request timed out after submission; its outcome is unknown. Check the provider before retrying a write"

  defp describe_error(:output_write_failed),
    do:
      "request completed but output could not be saved; choose a new writable path. Do not repeat a write request"

  defp describe_error(:deadline_exceeded),
    do: "reply budget expired before the next request could be submitted"

  defp describe_error(:output_exists), do: "output already exists; choose a new path"

  defp describe_error(reason) when reason in [:no_default, :not_found],
    do: "active identity or provider was not found; check your key and service address"

  defp describe_error({:remote, _code, message}) when is_binary(message),
    do: "provider reported an error: " <> safe_text(message)

  defp describe_error(reason) when is_atom(reason), do: Atom.to_string(reason)
  defp describe_error(_), do: "transport request failed"

  defp safe_text(message) do
    message
    |> String.slice(0, 500)
    |> String.replace(~r/[\x00-\x1f\x7f]/u, " ")
  end

  defp error(message) do
    IO.puts(:stderr, "Error: #{message}")
    Arc.CLI.Exit.halt(1)
  end

  defp help do
    IO.puts("""
    arc request <scheme+arc://provider-key/resource> --body TEXT | --input PATH
      --input -              Read exact bytes from stdin (at most 1 MiB)
      --output PATH          Save exact response bytes to a new file (default: stdout)
      --capability ID        Select a signed capability (default: primary)
      --timeout MS           Reply budget, 1..120000 milliseconds (default: 10000)
      --relay host:port      Use this relay, or ARC_RELAY
      --relay-pubkey KEY     Pin its public key, or ARC_RELAY_PUBKEY
      --local                Explicitly use same-host delivery instead of a relay
      --direct-policy PATH   Allow only listed direct scopes (requires a pinned relay)

    Request/reply only. The scheme must match the signed provider capability.
    Direct delivery is off by default. A matching policy may disclose its approved
    address to that peer. Failed requests are never automatically retried. A timeout
    may follow a write.
    """)
  end
end
