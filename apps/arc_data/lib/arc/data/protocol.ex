defmodule Arc.Data.Protocol do
  @moduledoc """
  Bounded request transport for `<scheme>+arc://<provider-public-key>/<resource>`.

  Uses an existing citizen agent, including its relay-only or local delivery
  policy. The provider key in the address pins both discovery and application
  replies. A signed capability supplies the method and body contract; the URI
  supplies the resource path. Payloads are opaque to this module.

  This first transport profile is request/reply. It never retries a request.
  A timeout after sending an application request has an unknown outcome.
  Long-lived ordered byte streams are a separate future profile.
  """

  alias Arc.Data.Agent
  alias Arc.Data.CapabilityManifest
  alias Arc.Data.CapabilityPackage
  alias Arc.Data.Direct
  alias Arc.Data.Frame

  @max_body_bytes 1_048_576
  @max_manifest_bytes 262_144
  @default_timeout_ms 10_000

  @spec max_body_bytes() :: pos_integer()
  def max_body_bytes, do: @max_body_bytes

  @spec parse(String.t()) :: {:ok, map()} | {:error, atom()}
  def parse(address) when is_binary(address) and byte_size(address) <= 4_096 do
    uri = URI.parse(address)

    with [_, scheme] <- Regex.run(~r/\A([a-z][a-z0-9-]*)\+arc\z/, uri.scheme || ""),
         true <- is_binary(uri.host) and Regex.match?(~r/\A[0-9a-fA-F]{64}\z/, uri.host),
         true <- is_nil(uri.userinfo) and is_nil(uri.port),
         true <- is_nil(uri.query) and is_nil(uri.fragment),
         {:ok, key} <- Base.decode16(uri.host, case: :mixed),
         {:ok, path} <- resource_path(uri.path) do
      {:ok, %{scheme: scheme, provider: Base.encode16(key, case: :lower), key: key, path: path}}
    else
      _ -> {:error, :invalid_protocol_uri}
    end
  rescue
    _ -> {:error, :invalid_protocol_uri}
  end

  def parse(_), do: {:error, :invalid_protocol_uri}

  @spec request(pid(), String.t(), binary(), keyword()) ::
          {:ok, %{body: binary(), meta: map()}} | {:error, term()}
  def request(agent, address, body, opts \\ [])

  def request(agent, address, body, opts) when is_pid(agent) and is_binary(body) do
    timeout = Keyword.get(opts, :timeout_ms, @default_timeout_ms)
    capability_id = Keyword.get(opts, :capability, "primary")

    with :ok <- validate_options(body, timeout, capability_id),
         deadline <- System.monotonic_time(:millisecond) + timeout,
         {:ok, target} <- parse(address),
         {:ok, package, generation} <- prepare_route(agent, target, capability_id, body, deadline) do
      capability = package["capability"]

      meta = %{
        "method" => get_in(capability, ["invocation", "method"]),
        "path" => target.path,
        "capability_id" => capability_id
      }

      case exchange(agent, target, body, meta, deadline, @max_body_bytes, generation) do
        {:error, reason} when reason in [:timeout, :delivery_unknown] ->
          {:error, :outcome_unknown}

        result ->
          result
      end
    end
  catch
    :exit, _ -> {:error, :transport_unavailable}
  end

  def request(_, _, _, _), do: {:error, :invalid_request}

  defp prepare_route(agent, target, capability_id, body, deadline) do
    manager = Agent.direct(agent)
    direct = if manager, do: Direct.route(manager, target, capability_id)

    case direct do
      %{package: package, generation: generation} ->
        with :ok <- validate_request(package["capability"], target, body),
             do: {:ok, package, generation}

      _ ->
        remaining = max(1, deadline - System.monotonic_time(:millisecond))

        with {:ok, entry} <- Agent.connect(agent, target.provider, min(11_000, remaining)),
             true <- entry.public_key == target.key or {:error, :provider_mismatch},
             {:ok, package} <- fetch_capability(agent, target, capability_id, deadline),
             :ok <- validate_request(package["capability"], target, body) do
          generation = maybe_promote(manager, target, package, entry, deadline)
          {:ok, package, generation}
        end
    end
  end

  defp maybe_promote(nil, _target, _package, _entry, _deadline), do: :relay

  defp maybe_promote(manager, target, package, entry, deadline) do
    remaining = deadline - System.monotonic_time(:millisecond)
    # Leave an application budget. A failed optimization cannot extend the
    # caller's deadline or submit the same request on two paths.
    if remaining > 500 and Direct.allowed?(manager, target, package["capability"]["id"]) do
      case Direct.promote(
             manager,
             target,
             package,
             entry.x25519_public,
             min(3_000, div(remaining, 2))
           ) do
        {:ok, %{generation: generation}} -> generation
        _ -> :relay
      end
    else
      :relay
    end
  end

  defp resource_path(path) do
    path = path || "/"

    if String.starts_with?(path, "/") and byte_size(path) <= 2_048 and
         Regex.match?(~r/\A[A-Za-z0-9._~\/-]+\z/, path) and
         not Enum.any?(String.split(path, "/"), &(&1 in [".", ".."])) and
         path != "/info" and not String.starts_with?(path, "/info/") do
      {:ok, path}
    else
      {:error, :invalid_resource_path}
    end
  end

  defp validate_options(body, timeout, capability_id) do
    cond do
      byte_size(body) > @max_body_bytes ->
        {:error, :request_too_large}

      not is_integer(timeout) or timeout < 1 or timeout > 120_000 ->
        {:error, :invalid_timeout}

      not is_binary(capability_id) ->
        {:error, :invalid_capability_id}

      not Regex.match?(~r/\A[a-zA-Z0-9_-]{1,64}\z/, capability_id) ->
        {:error, :invalid_capability_id}

      true ->
        :ok
    end
  end

  defp fetch_capability(agent, target, capability_id, deadline) do
    meta = %{"method" => "GET", "path" => CapabilityManifest.detail_path(capability_id)}

    with {:ok, %{body: body}} <- exchange(agent, target, "", meta, deadline, @max_manifest_bytes),
         {:ok, document} <- decode_document(body),
         {:ok, package} <- CapabilityPackage.verify(document),
         true <-
           get_in(package, ["provider", "public_key"]) == target.provider or
             {:error, :provider_mismatch},
         true <-
           get_in(package, ["capability", "id"]) == capability_id or
             {:error, :capability_mismatch} do
      {:ok, package}
    else
      {:error, :delivery_unknown} -> {:error, :transport_unavailable}
      error -> error
    end
  end

  defp decode_document(body) do
    {:ok, :json.decode(body)}
  rescue
    _ -> {:error, :invalid_capability_document}
  end

  defp validate_request(capability, target, body) do
    invocation = capability["invocation"]

    cond do
      capability["scheme"] != target.scheme ->
        {:error, :scheme_mismatch}

      invocation["mode"] != "request_reply" ->
        {:error, :unsupported_transport_profile}

      true ->
        validate_body_contract(invocation, body)
    end
  end

  defp validate_body_contract(%{"request_body" => request, "response_body" => response}, body)
       when is_map(request) and is_map(response) do
    request_encoding = request["encoding"]
    response_encoding = response["encoding"]

    cond do
      request_encoding not in [nil, "utf-8", "base64"] ->
        {:error, :unsupported_encoding}

      response_encoding not in [nil, "utf-8", "base64"] ->
        {:error, :unsupported_encoding}

      request_encoding != "base64" and not String.valid?(body) ->
        {:error, :binary_encoding_required}

      true ->
        :ok
    end
  end

  defp validate_body_contract(_, _), do: {:error, :invalid_body_contract}

  defp exchange(agent, target, body, meta, deadline, limit, generation \\ :relay) do
    if System.monotonic_time(:millisecond) >= deadline do
      {:error, :deadline_exceeded}
    else
      request_id = Frame.new_request_id()

      opts = [request_id: request_id, meta: meta, track_reply: true, deadline_ms: deadline]

      opts =
        if generation == :relay, do: opts, else: Keyword.put(opts, :direct_generation, generation)

      try do
        with :ok <- Agent.send_message(agent, target.provider, body, opts) do
          await_reply(agent, target.key, request_id, deadline, limit, generation)
        end
      catch
        :exit, _ -> {:error, :delivery_unknown}
      after
        Agent.finish_request(agent, target.key, request_id)
      end
    end
  end

  defp await_reply(agent, key, request_id, deadline, limit, generation) do
    Agent.poll_mailbox(agent)

    matcher = fn message ->
      message[:from_key] == key and message[:request_id] == request_id and
        message[:kind] in [:response, :error] and
        Map.get(message, :route_generation, :relay) == generation
    end

    case Agent.take_inbox(agent, matcher) do
      [reply] -> decode_reply(reply, limit)
      [] -> wait_for_reply(agent, key, request_id, deadline, limit, generation)
    end
  end

  defp wait_for_reply(agent, key, request_id, deadline, limit, generation) do
    remaining = deadline - System.monotonic_time(:millisecond)

    if remaining <= 0 do
      {:error, :timeout}
    else
      Process.sleep(min(remaining, 10))
      await_reply(agent, key, request_id, deadline, limit, generation)
    end
  end

  defp decode_reply(%{text: body}, limit) when byte_size(body) > limit,
    do: {:error, :response_too_large}

  defp decode_reply(%{kind: :error} = reply, _limit),
    do:
      {:error,
       {:remote, reply[:error_code] || "error", reply[:error_message] || "provider error"}}

  defp decode_reply(%{kind: :response, text: body, meta: meta}, _limit),
    do: {:ok, %{body: body, meta: meta}}
end
