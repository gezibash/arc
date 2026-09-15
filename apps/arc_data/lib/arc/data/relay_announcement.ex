defmodule Arc.Data.RelayAnnouncement do
  @moduledoc """
  Signed, short-lived provider announcements carried by ARC relays.

  An announcement is deliberately a small public document.  It says which
  identity is offering capabilities and where their signed capability detail
  can be requested; it never includes a private key or an endpoint chosen by
  an untrusted caller.
  """

  alias Arc.Identity

  @version_v1 1
  @version_v2 2
  @version_v3 3
  @default_ttl 180
  @max_ttl 180
  @max_future_skew 30
  @max_record_bytes 8_192
  @max_capabilities 8

  @capability_limits %{
    "id" => 128,
    "kind" => 128,
    "scheme" => 128,
    "title" => 256,
    "summary" => 1_024,
    "invocation_mode" => 256,
    "release_version" => 256,
    "channel" => 256,
    "detail_path" => 256
  }

  @v1_record_keys ~w(version public_key x25519_public capabilities issued_at expires_at signature)
  @v2_record_keys ~w(version public_key x25519_public capabilities issued_at expires_at federation relay_public_key signature)
  @v3_record_keys @v2_record_keys
  @v1_domain "arc-relay-announcement-v1\n"
  @v2_domain "arc-relay-announcement-v2\n"
  @v3_domain "arc-relay-announcement-v3\n"

  @type entry :: %{
          public_key: <<_::256>>,
          x25519_public: <<_::256>>,
          name: String.t(),
          short_name: String.t(),
          capabilities: [map()],
          issued_at: integer(),
          expires_at: integer(),
          federation: :local | :direct | :network,
          relay_public_key: <<_::256>> | nil,
          record: map()
        }

  @doc """
  Creates an announcement signed by `identity`.

  `capabilities` may be the list returned inside `CapabilityManifest.summary/2`
  or that summary document itself.  `:now` is primarily useful for reproducible
  tests; `:ttl` is capped at #{@max_ttl} seconds.
  """
  @spec create(Identity.t(), [map()] | map(), keyword()) :: map()
  def create(%Identity{} = identity, capabilities, opts \\ []) do
    now = Keyword.get(opts, :now, System.system_time(:second))
    ttl = Keyword.get(opts, :ttl, @default_ttl)

    unless is_integer(now) and is_integer(ttl) and ttl > 0 and ttl <= @max_ttl do
      raise ArgumentError, "announcement time must be an integer and ttl must be 1..#{@max_ttl}"
    end

    capabilities = capability_list!(capabilities)

    if length(capabilities) > @max_capabilities do
      raise ArgumentError, "announcement supports at most #{@max_capabilities} capabilities"
    end

    capabilities = Enum.map(capabilities, &normalize_capability!/1)

    unsigned = %{
      "version" => @version_v1,
      "public_key" => Identity.encode_public_key(identity),
      "x25519_public" =>
        identity |> Identity.to_x25519() |> elem(0) |> Identity.encode_public_key(),
      "capabilities" => capabilities,
      "issued_at" => now,
      "expires_at" => now + ttl
    }

    unsigned = federation_fields!(unsigned, opts)

    signature =
      unsigned
      |> signing_payload(Map.fetch!(unsigned, "version"))
      |> then(&Identity.sign(identity, &1))
      |> Base.encode16(case: :lower)

    record = Map.put(unsigned, "signature", signature)

    if byte_size(canonical(record)) > @max_record_bytes do
      raise ArgumentError, "announcement exceeds #{@max_record_bytes} bytes"
    end

    record
  end

  @doc "Verifies a signed announcement and its short lifetime."
  @spec verify(map(), keyword()) :: {:ok, entry()} | {:error, :invalid_announcement}
  def verify(record, opts \\ [])

  def verify(record, opts) when is_map(record) do
    now = Keyword.get(opts, :now, System.system_time(:second))

    with true <- is_integer(now),
         {:ok, unsigned, public_key, x25519_public, signature, capabilities, issued_at,
          expires_at, federation, relay_public_key, version} <-
           validate_shape(record),
         true <- byte_size(canonical(record)) <= @max_record_bytes,
         true <- issued_at <= now + @max_future_skew,
         true <- expires_at > now,
         true <- expires_at - issued_at > 0 and expires_at - issued_at <= @max_ttl,
         true <- Identity.verify(public_key, signing_payload(unsigned, version), signature) do
      {:ok,
       %{
         public_key: public_key,
         x25519_public: x25519_public,
         name: Identity.name(public_key),
         short_name: Identity.short_name(public_key),
         capabilities: capabilities,
         issued_at: issued_at,
         expires_at: expires_at,
         federation: federation,
         relay_public_key: relay_public_key,
         record: record
       }}
    else
      _ -> {:error, :invalid_announcement}
    end
  rescue
    _ -> {:error, :invalid_announcement}
  end

  def verify(_, _opts), do: {:error, :invalid_announcement}

  @doc "Returns whether this opt-in announcement may leave its home relay."
  @spec federatable?(entry(), <<_::256>>) :: boolean()
  def federatable?(
        %{federation: federation, relay_public_key: relay_public_key},
        <<_::binary-size(32)>> = origin_relay_key
      )
      when federation in [:direct, :network] do
    relay_public_key == origin_relay_key
  end

  def federatable?(_entry, _origin_relay_key), do: false

  @doc "Matches a provider identity by full petname, short petname, or public-key prefix."
  @spec matches?(entry(), String.t()) :: boolean()
  def matches?(entry, query) when is_map(entry) and is_binary(query) and query != "" do
    if not String.valid?(query) do
      false
    else
      query = String.downcase(query)
      public_key = entry |> Map.get(:public_key) |> public_key_hex()

      query == String.downcase(to_string(Map.get(entry, :name, ""))) or
        query == String.downcase(to_string(Map.get(entry, :short_name, ""))) or
        (is_binary(public_key) and String.starts_with?(public_key, query))
    end
  end

  def matches?(_entry, _query), do: false

  @doc "Matches provider names and public capability summary fields for relay search."
  @spec search_match?(entry(), String.t()) :: boolean()
  def search_match?(entry, query) when is_map(entry) and is_binary(query) do
    capabilities = Map.get(entry, :capabilities, [])

    if String.valid?(query) and is_list(capabilities) do
      tokens = query |> String.downcase() |> String.split(~r/\s+/, trim: true)

      case tokens do
        [] -> capabilities != []
        _ -> Enum.any?(capabilities, &capability_matches?(entry, &1, tokens))
      end
    else
      false
    end
  end

  def search_match?(_entry, _query), do: false

  @doc "Returns the identity shape shared by capability manifests and discovery."
  @spec provider(entry()) :: map()
  def provider(%{public_key: public_key, name: name, short_name: short_name}) do
    %{
      "public_key" => Identity.encode_public_key(public_key),
      "name" => name,
      "short_name" => short_name
    }
  end

  defp capability_list!(%{"capabilities" => capabilities}) when is_list(capabilities),
    do: capabilities

  defp capability_list!(capabilities) when is_list(capabilities), do: capabilities

  defp capability_list!(_),
    do: raise(ArgumentError, "capabilities must be a list or manifest summary")

  defp normalize_capability!(capability) when is_map(capability) do
    keys = Map.keys(capability)

    unless Enum.all?(keys, &(&1 in Map.keys(@capability_limits))) do
      raise ArgumentError, "capability contains unsupported fields"
    end

    normalized =
      Enum.reduce(@capability_limits, %{}, fn {key, limit}, acc ->
        case Map.get(capability, key) do
          value when key != "id" and value in [nil, ""] ->
            acc

          value when is_binary(value) ->
            if valid_capability_string?(value, limit),
              do: Map.put(acc, key, value),
              else: raise(ArgumentError, "invalid capability #{key}")

          _ ->
            raise ArgumentError, "invalid capability #{key}"
        end
      end)

    if Map.has_key?(normalized, "id"),
      do: normalized,
      else: raise(ArgumentError, "capability id is required")
  end

  defp normalize_capability!(_), do: raise(ArgumentError, "capability must be a map")

  defp federation_fields!(unsigned, opts) do
    case Keyword.get(opts, :federation, :local) do
      :local ->
        if Keyword.has_key?(opts, :relay_public_key) do
          raise ArgumentError, "relay_public_key requires a federated announcement"
        end

        unsigned

      federation when federation in [:direct, :network] ->
        relay_public_key = Keyword.get(opts, :relay_public_key)

        unless is_binary(relay_public_key) and byte_size(relay_public_key) == 32 do
          raise ArgumentError, "relay_public_key must be a 32-byte binary"
        end

        unsigned
        |> Map.put("version", federation_version(federation))
        |> Map.put("federation", Atom.to_string(federation))
        |> Map.put("relay_public_key", Identity.encode_public_key(relay_public_key))

      _ ->
        raise ArgumentError, "federation must be :local, :direct, or :network"
    end
  end

  defp validate_shape(record) do
    with {:ok, version, federation, relay_public_key, record_keys} <- validate_version(record),
         true <- Map.keys(record) |> MapSet.new() == MapSet.new(record_keys),
         {:ok, public_key} <- decode_hex(record["public_key"], 32),
         {:ok, x25519_public} <- decode_hex(record["x25519_public"], 32),
         {:ok, signature} <- decode_hex(record["signature"], 64),
         true <- is_integer(record["issued_at"]),
         true <- is_integer(record["expires_at"]),
         {:ok, capabilities} <- validate_capabilities(record["capabilities"]) do
      unsigned = Map.take(record, record_keys -- ["signature"])

      {:ok, unsigned, public_key, x25519_public, signature, capabilities, record["issued_at"],
       record["expires_at"], federation, relay_public_key, version}
    else
      _ -> :error
    end
  end

  defp validate_version(%{"version" => @version_v1}),
    do: {:ok, @version_v1, :local, nil, @v1_record_keys}

  defp validate_version(%{"version" => @version_v2, "federation" => "direct"} = record) do
    with {:ok, relay_public_key} <- decode_hex(record["relay_public_key"], 32) do
      {:ok, @version_v2, :direct, relay_public_key, @v2_record_keys}
    else
      _ -> :error
    end
  end

  defp validate_version(%{"version" => @version_v3, "federation" => "network"} = record) do
    with {:ok, relay_public_key} <- decode_hex(record["relay_public_key"], 32) do
      {:ok, @version_v3, :network, relay_public_key, @v3_record_keys}
    else
      _ -> :error
    end
  end

  defp validate_version(_), do: :error

  defp validate_capabilities(capabilities)
       when is_list(capabilities) and length(capabilities) <= @max_capabilities do
    Enum.reduce_while(capabilities, {:ok, []}, fn capability, {:ok, valid} ->
      case validate_capability(capability) do
        {:ok, capability} -> {:cont, {:ok, [capability | valid]}}
        :error -> {:halt, :error}
      end
    end)
    |> case do
      {:ok, valid} -> {:ok, Enum.reverse(valid)}
      :error -> :error
    end
  end

  defp validate_capabilities(_), do: :error

  defp validate_capability(capability) when is_map(capability) do
    keys = Map.keys(capability)

    if Enum.all?(keys, &(&1 in Map.keys(@capability_limits))) and Map.has_key?(capability, "id") and
         Enum.all?(capability, fn {key, value} ->
           is_binary(key) and is_binary(value) and String.valid?(value) and
             byte_size(value) <= Map.fetch!(@capability_limits, key) and
             (key != "id" or value != "")
         end) do
      {:ok, normalize_optional_empty(capability)}
    else
      :error
    end
  end

  defp validate_capability(_), do: :error

  defp normalize_optional_empty(capability) do
    capability
    |> Enum.reject(fn {key, value} -> key != "id" and value == "" end)
    |> Map.new()
  end

  defp valid_capability_string?(value, limit),
    do: value != "" and String.valid?(value) and byte_size(value) <= limit

  defp capability_matches?(entry, capability, tokens) when is_map(capability) do
    haystack =
      [Map.get(entry, :name, ""), Map.get(entry, :short_name, "") | Map.values(capability)]
      |> Enum.filter(&is_binary/1)
      |> Enum.join(" ")
      |> String.downcase()

    Enum.all?(tokens, &String.contains?(haystack, &1))
  end

  defp capability_matches?(_entry, _capability, _tokens), do: false

  defp decode_hex(value, expected_bytes)
       when is_binary(value) and byte_size(value) == expected_bytes * 2 do
    case Base.decode16(value, case: :lower) do
      {:ok, binary} when byte_size(binary) == expected_bytes -> {:ok, binary}
      _ -> :error
    end
  end

  defp decode_hex(_, _), do: :error

  defp signing_payload(unsigned, @version_v1), do: @v1_domain <> canonical(unsigned)
  defp signing_payload(unsigned, @version_v2), do: @v2_domain <> canonical(unsigned)
  defp signing_payload(unsigned, @version_v3), do: @v3_domain <> canonical(unsigned)

  defp federation_version(:direct), do: @version_v2
  defp federation_version(:network), do: @version_v3

  # Deterministic JSON avoids Erlang term serialization and keeps signatures
  # portable to non-Elixir relay implementations.
  defp canonical(value) when is_map(value) do
    body =
      value
      |> Map.keys()
      |> Enum.sort()
      |> Enum.map(fn key -> [canonical_scalar(key), ?:, canonical(Map.fetch!(value, key))] end)
      |> Enum.intersperse(?,)

    IO.iodata_to_binary([?{, body, ?}])
  end

  defp canonical(value) when is_list(value) do
    body = value |> Enum.map(&canonical/1) |> Enum.intersperse(?,)
    IO.iodata_to_binary([?[, body, ?]])
  end

  defp canonical(value), do: canonical_scalar(value)
  defp canonical_scalar(value), do: IO.iodata_to_binary(:json.encode(value))

  defp public_key_hex(<<_::binary-size(32)>> = public_key),
    do: Identity.encode_public_key(public_key)

  defp public_key_hex(_), do: nil
end
