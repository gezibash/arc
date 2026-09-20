defmodule Arc.CLI.Join do
  @moduledoc "Confirm a relay pin and persist a citizen's default relay."

  alias Arc.Identity
  alias Arc.Identity.KeyStore
  alias Arc.Net.{RelayConfig, RelayStatus}

  @usage "usage: arc join <host[:port]> [--relay-pubkey KEY]"

  def run(args) do
    if Enum.count(args, &(&1 == "--relay-pubkey" or String.starts_with?(&1, "--relay-pubkey="))) >
         1 do
      error(@usage)
    end

    case OptionParser.parse(args, strict: [help: :boolean, relay_pubkey: :string]) do
      {[help: true], [], []} ->
        IO.puts(@usage)

      {opts, [target], []} ->
        if length(Keyword.keys(opts)) == length(Enum.uniq(Keyword.keys(opts))) and
             not Keyword.has_key?(opts, :help),
           do: join(target, opts),
           else: error(@usage)

      _ ->
        error(@usage)
    end
  end

  defp join(target, opts) do
    with {:ok, address} <- RelayConfig.normalize_address(target),
         {:ok, config} <- RelayConfig.load(),
         {:ok, identity} <- identity_plan(),
         {:ok, pin} <- expected_pin(config, address, opts),
         {:ok, status} <- fetch(address, pin),
         :ok <- confirm(address, status, pin),
         {:ok, _verified} <- fetch(address, status["public_key"]),
         {:ok, identity} <- ensure_identity(identity),
         :ok <- RelayConfig.remember(address, status["public_key"]) do
      IO.puts("Joined #{address}")
      IO.puts("Identity: #{Identity.name(identity)}")
      IO.puts("Default relay saved. Future ARC commands connect through this relay.")
      IO.puts("Try: arc status, arc discover")
      explain_override(address, status["public_key"])
      :ok
    else
      {:error, reason} -> error(describe_error(reason))
    end
  end

  defp identity_plan do
    case KeyStore.resolve_active() do
      {:ok, identity} -> {:ok, identity}
      {:error, :no_default} -> fresh_identity_plan()
      {:error, reason} -> {:error, {:identity, reason}}
    end
  end

  defp fresh_identity_plan do
    if KeyStore.list() == [],
      do: {:ok, :new},
      else: {:error, {:identity, :no_default}}
  end

  defp ensure_identity(:new) do
    identity = Identity.generate()

    with :ok <- KeyStore.save(identity),
         :ok <- KeyStore.set_default(Identity.name(identity)) do
      {:ok, identity}
    else
      {:error, reason} -> {:error, {:identity, reason}}
    end
  end

  defp ensure_identity(%Identity{} = identity), do: {:ok, identity}

  defp expected_pin(config, address, opts) do
    known = config["relays"][address]
    supplied = opts[:relay_pubkey]
    parsed = if supplied, do: Arc.Net.relay_pubkey_from(supplied)
    canonical = if parsed, do: Base.encode16(parsed, case: :lower)

    cond do
      supplied != nil and parsed == nil ->
        {:error, :invalid_relay_pubkey_pin}

      canonical != nil and known != nil and canonical != known ->
        {:error, :relay_pubkey_mismatch}

      true ->
        {:ok, known || canonical}
    end
  end

  defp fetch(address, pin) do
    {host, port} = Arc.Net.relay_address_from(address)
    RelayStatus.fetch(host, port, if(pin, do: Arc.Net.relay_pubkey_from(pin)))
  end

  defp confirm(address, status, nil) do
    IO.puts("Relay: #{address}")
    IO.puts("Public-key fingerprint: #{status["public_key"]}")
    IO.puts("Compare this fingerprint with the relay operator before trusting it.")

    case IO.gets("Trust and remember this relay? [y/N] ") do
      answer when is_binary(answer) ->
        if String.downcase(String.trim(answer)) in ["y", "yes"],
          do: :ok,
          else: {:error, :trust_not_confirmed}

      _ ->
        {:error, :trust_not_confirmed}
    end
  end

  defp confirm(_address, _status, _pin), do: :ok

  defp explain_override(address, pin) do
    case RelayConfig.resolve() do
      {:ok, %{address: ^address, public_key: ^pin}} ->
        :ok

      _ ->
        IO.puts("Your environment overrides the saved default. To use this join in this shell:")
        IO.puts("  unset ARC_RELAY ARC_RELAY_PUBKEY")
        IO.puts("Remove conflicting exports from shell startup settings for future shells.")
    end
  end

  defp describe_error({:identity, reason}), do: Arc.CLI.Keys.describe_error(reason)
  defp describe_error(:invalid_relay_address), do: "Use a hostname or host:port."

  defp describe_error(:invalid_relay_pubkey_pin),
    do: "The supplied relay public key is invalid."

  defp describe_error(:relay_pubkey_mismatch),
    do: "Relay key changed or differs from the supplied pin. Saved trust was not replaced."

  defp describe_error(:trust_not_confirmed), do: "Join cancelled; relay trust was not saved."
  defp describe_error(:invalid_relay_config), do: "Cannot read or save relay configuration."
  defp describe_error(:timeout), do: "Relay connection timed out."
  defp describe_error(:econnrefused), do: "Relay connection refused."
  defp describe_error(:nxdomain), do: "Relay hostname could not be resolved."
  defp describe_error(:unsupported), do: "This relay must be upgraded to support joining."
  defp describe_error(_), do: "Could not join the relay; configuration was not completed."

  @spec error(String.t()) :: no_return()
  defp error(message) do
    IO.puts(:stderr, "error: #{message}")
    Arc.CLI.Exit.halt(1)
  end
end
