defmodule Arc.Net do
  @moduledoc """
  Public API for Arc network transport.

  Provides relay-mesh delivery as a tier between in-process AgentRegistry
  and file-based mailbox fallback. Application packets remain end-to-end
  encrypted; relay directory announcements and queries are public metadata.
  """

  alias Arc.Data.Packet
  alias Arc.Net.Transport
  alias Arc.Net.TransportManager

  @default_directory_limit 10
  @max_directory_limit 50

  @doc """
  Deliver a packet via the source identity's relay transport.
  Falls back to the file mailbox when no relay transport is available.
  """
  def deliver(from_pk, to_pk, packet) when is_binary(from_pk) do
    TransportManager.deliver(from_pk, to_pk, packet)
  end

  def deliver(to_pk, packet) do
    case Packet.decode(packet) do
      {:ok, %{src: from_pk}} -> TransportManager.deliver(from_pk, to_pk, packet)
      {:error, _reason} -> TransportManager.deliver(nil, to_pk, packet)
    end
  end

  @doc "Publish a signed, public provider announcement through the source identity's relay."
  def announce(source_pubkey, signed_record) when is_binary(source_pubkey) do
    with {:ok, transport} <- relay_transport(source_pubkey),
         {:ok, %{"ok" => true}} <-
           Arc.Net.Transport.directory_request(transport, :announce, %{"record" => signed_record}) do
      :ok
    else
      {:ok, %{"ok" => false, "error" => reason}} -> {:error, directory_error(reason)}
      {:ok, _} -> {:error, :relay_discovery_unavailable}
      {:error, _} = error -> error
    end
  end

  @doc "Resolve one identity through the source identity's connected relay."
  def resolve_via_relay(source_pubkey, query) when is_binary(source_pubkey) do
    with {:ok, transport} <- relay_transport(source_pubkey),
         {:ok, %{"ok" => true, "entries" => records}} <-
           Arc.Net.Transport.directory_request(transport, :resolve, %{"query" => query}),
         true <- is_list(records) and length(records) <= 2,
         {:ok, entries} <- validate_directory_entries(records, query, :resolve) do
      {:ok, entries}
    else
      {:ok, %{"ok" => false, "error" => reason}} -> {:error, directory_error(reason)}
      {:ok, _} -> {:error, :relay_discovery_unavailable}
      {:error, _} = error -> error
    end
  end

  @doc "Search public provider announcements through the connected relay."
  def discover_via_relay(source_pubkey, query, opts \\ [])
      when is_binary(source_pubkey) and is_list(opts) do
    limit = opts |> Keyword.get(:limit, @default_directory_limit) |> normalize_directory_limit()
    after_cursor = Keyword.get(opts, :after)

    fields =
      %{"query" => query, "limit" => limit}
      |> maybe_put_after(after_cursor)

    with {:ok, transport} <- relay_transport(source_pubkey),
         {:ok, reply} <-
           Arc.Net.Transport.directory_request(transport, :search, fields) do
      validate_relay_discovery_reply(reply, query, after_cursor, limit)
    end
  end

  defp validate_relay_discovery_reply(reply, query, after_cursor, limit) do
    with %{"ok" => true, "entries" => records, "total" => total} <- reply,
         true <- valid_directory_page?(reply, records, total, limit),
         {:ok, next} <- validate_cursor(Map.get(reply, "next")),
         {:ok, entries} <- validate_directory_entries(records, query, :search),
         true <- valid_page?(entries, after_cursor, next) do
      {:ok, discovery_result(reply, entries, next, total)}
    else
      %{"ok" => false, "error" => reason} -> {:error, directory_error(reason)}
      _ -> {:error, :relay_discovery_unavailable}
    end
  end

  defp valid_directory_page?(reply, records, total, limit) do
    is_list(records) and is_integer(total) and total >= 0 and length(records) <= limit and
      is_boolean(Map.get(reply, "partial", false)) and is_boolean(Map.get(reply, "cached", false))
  end

  defp discovery_result(reply, entries, next, total) do
    %{entries: entries, next: next, total: total}
    |> maybe_put_discovery_flag(reply, "partial", :partial?)
    |> maybe_put_discovery_flag(reply, "cached", :cached?)
  end

  defp maybe_put_discovery_flag(result, reply, key, result_key) do
    if Map.has_key?(reply, key), do: Map.put(result, result_key, reply[key]), else: result
  end

  @doc "Deliver only through a live relay connection; never use local mailbox fallback."
  def deliver_via_relay(source_pubkey, to_pubkey, packet)
      when is_binary(source_pubkey) and is_binary(to_pubkey) and is_binary(packet) do
    with {:ok, transport} <- relay_transport(source_pubkey) do
      Arc.Net.Transport.deliver_strict(transport, to_pubkey, packet)
    end
  end

  @doc "Return the public key presented by this identity's currently selected relay."
  def relay_public_key(source_pubkey) when is_binary(source_pubkey) do
    with {:ok, transport} <- relay_transport(source_pubkey) do
      Arc.Net.Transport.relay_public_key(transport)
    end
  end

  @doc "Return the live connection state for this identity's existing relay transport."
  @spec relay_status(binary()) ::
          {:ok,
           %{
             required(:status) => :connected | :reconnecting | :disconnected,
             optional(:host) => String.t(),
             optional(:port) => pos_integer()
           }}
          | {:error, :relay_not_connected | :relay_status_unavailable}
  def relay_status(source_pubkey) when is_binary(source_pubkey) do
    case TransportManager.lookup(source_pubkey, 500) do
      {:ok, transport} -> Transport.relay_status(transport)
      :error -> {:error, :relay_not_connected}
      {:error, :unavailable} -> {:error, :relay_status_unavailable}
    end
  end

  @doc "Return this identity's current relay-observed endpoint for direct-promotion setup."
  def relay_endpoint(source_pubkey) when is_binary(source_pubkey) do
    with {:ok, transport} <- relay_transport(source_pubkey),
         {:ok, %{connection: conn, local: local}} <- Transport.relay_endpoint_context(transport),
         {:ok, %{"ok" => true, "observed" => observed}} <-
           Arc.Net.Transport.directory_request(transport, :observe, %{}),
         true <- valid_observed_endpoint?(observed),
         true <- Transport.relay_endpoint_current?(transport, conn) do
      {:ok, %{connection: conn, local: local, observed: observed}}
    else
      {:ok, %{"ok" => false}} -> {:error, :relay_observation_unavailable}
      {:error, _} = error -> error
      _ -> {:error, :relay_observation_unavailable}
    end
  end

  @doc "Whether an observed relay connection remains the source identity's current connection."
  def relay_endpoint_current?(source_pubkey, conn_pid)
      when is_binary(source_pubkey) and is_pid(conn_pid) do
    case relay_transport(source_pubkey) do
      {:ok, transport} -> Transport.relay_endpoint_current?(transport, conn_pid)
      _ -> false
    end
  end

  @doc "Open an outbound TCP connection to a relay node with identity authentication."
  def connect_relay(host, port, my_identity) do
    TransportManager.connect_relay(host, port, my_identity)
  end

  @doc "Open an outbound TCP connection with optional relay pubkey pinning."
  def connect_relay(host, port, my_identity, relay_pubkey_pin) do
    TransportManager.connect_relay(host, port, my_identity, relay_pubkey_pin)
  end

  @doc """
  Acquire a leased relay transport for one local owner process.
  The transport is identity-scoped and reaped after the last owner disappears
  and the idle timeout expires.
  """
  def acquire_relay(host, port, my_identity) do
    TransportManager.acquire(host, port, my_identity)
  end

  def acquire_relay(host, port, my_identity, relay_pubkey_pin) do
    TransportManager.acquire(host, port, my_identity, relay_pubkey_pin)
  end

  def acquire_relay(host, port, my_identity, relay_pubkey_pin, opts) when is_list(opts) do
    TransportManager.acquire(host, port, my_identity, relay_pubkey_pin, opts)
  end

  @doc "Release a leased relay transport owner."
  def release_relay(identity_or_pubkey) do
    TransportManager.release(identity_or_pubkey)
  end

  def release_relay(identity_or_pubkey, owner_pid) when is_pid(owner_pid) do
    TransportManager.release(identity_or_pubkey, owner_pid)
  end

  @doc """
  Parse ARC_RELAY env var into `{charlist_host, integer_port}`.
  Returns nil if unset or malformed.
  """
  def relay_address do
    case System.get_env("ARC_RELAY") do
      nil -> nil
      addr -> parse_host_port(addr)
    end
  end

  @doc "Parse a `host:port` string into `{charlist_host, integer_port}` or nil."
  def relay_address_from(addr) when is_binary(addr), do: parse_host_port(addr)
  def relay_address_from(_), do: nil

  @doc """
  Parse ARC_RELAY_PUBKEY env var into a 32-byte pubkey binary.
  Supports hex (with optional 0x prefix) or base64.
  Returns nil if unset or malformed.
  """
  def relay_pubkey do
    case System.get_env("ARC_RELAY_PUBKEY") do
      nil -> nil
      value -> parse_pubkey(value)
    end
  end

  @doc "Parse a relay pubkey string into 32-byte binary or nil."
  def relay_pubkey_from(value) when is_binary(value), do: parse_pubkey(value)
  def relay_pubkey_from(_), do: nil

  @max_frame_header_bytes 0xFFFF_FFFF

  @doc """
  Parse a frame cap string into `:unbounded` or a positive byte count.

  Accepts a decimal integer, `0`, or `unbounded`. `0` means no cap. The value
  cannot exceed the 32-bit frame header. Returns `:error` on any other input.
  """
  @spec parse_frame_cap(String.t() | nil) :: {:ok, :unbounded | pos_integer()} | :error
  def parse_frame_cap(nil), do: :error

  def parse_frame_cap(value) when is_binary(value) do
    case String.trim(value) do
      "unbounded" ->
        {:ok, :unbounded}

      "0" ->
        {:ok, :unbounded}

      text ->
        case Integer.parse(text) do
          {n, ""} when n > 0 and n <= @max_frame_header_bytes -> {:ok, n}
          _ -> :error
        end
    end
  end

  @doc """
  Set the frame cap for every connection this node opens or accepts.
  Reads `ARC_RELAY_MAX_FRAME_BYTES` when `value` is nil.
  Returns `:ok`, `{:error, :invalid}` on bad input, or `:unset` when nothing was given.
  """
  @spec configure_frame_cap(String.t() | nil) :: :ok | :unset | {:error, :invalid}
  def configure_frame_cap(value) do
    case value || System.get_env("ARC_RELAY_MAX_FRAME_BYTES") do
      nil ->
        :unset

      text ->
        case parse_frame_cap(text) do
          {:ok, cap} ->
            Application.put_env(:arc_net, :max_frame_bytes, cap)
            :ok

          :error ->
            {:error, :invalid}
        end
    end
  end

  defp parse_host_port(addr) do
    case URI.parse("//" <> addr) do
      %URI{host: host, port: port}
      when is_binary(host) and host != "" and is_integer(port) and port > 0 and port < 65_536 ->
        {String.to_charlist(host), port}

      _ ->
        nil
    end
  end

  defp relay_transport(source_pubkey) do
    case TransportManager.lookup(source_pubkey) do
      {:ok, transport} -> {:ok, transport}
      :error -> {:error, :relay_not_connected}
    end
  end

  defp valid_observed_endpoint?(%{"host" => host, "port" => port} = endpoint)
       when map_size(endpoint) == 2 do
    is_binary(host) and valid_observed_ip?(host) and is_integer(port) and port in 1..65_535
  end

  defp valid_observed_endpoint?(_), do: false

  defp valid_observed_ip?(host) do
    case :inet.parse_address(String.to_charlist(host)) do
      {:ok, _ip} -> true
      {:error, _reason} -> false
    end
  end

  defp normalize_directory_limit(value) when is_integer(value) and value > 0,
    do: min(value, @max_directory_limit)

  defp normalize_directory_limit(_), do: @default_directory_limit
  defp null_to_nil(:null), do: nil
  defp null_to_nil(value), do: value

  defp maybe_put_after(fields, cursor) when is_binary(cursor),
    do: Map.put(fields, "after", cursor)

  defp maybe_put_after(fields, _), do: fields

  defp validate_cursor(value) do
    value = null_to_nil(value)

    if is_nil(value) or (is_binary(value) and Regex.match?(~r/\A[0-9a-f]{64}\z/, value)) do
      {:ok, value}
    else
      :error
    end
  end

  defp validate_directory_entries(records, query, mode)
       when is_list(records) and is_binary(query) do
    now = System.system_time(:second)

    Enum.reduce_while(records, {:ok, []}, fn record, {:ok, entries} ->
      case Arc.Data.RelayAnnouncement.verify(record, now: now) do
        {:ok, entry} ->
          matches? =
            case mode do
              :resolve -> Arc.Data.RelayAnnouncement.matches?(entry, query)
              :search -> Arc.Data.RelayAnnouncement.search_match?(entry, query)
            end

          if matches?, do: {:cont, {:ok, [entry | entries]}}, else: {:halt, :error}

        _ ->
          {:halt, :error}
      end
    end)
    |> case do
      {:ok, entries} -> {:ok, Enum.reverse(entries)}
      :error -> {:error, :invalid_relay_announcement}
    end
  end

  defp validate_directory_entries(_, _, _), do: {:error, :invalid_relay_announcement}

  defp valid_page?(entries, after_cursor, next) do
    cursors = Enum.map(entries, fn entry -> Base.encode16(entry.public_key, case: :lower) end)

    cursors == Enum.sort(cursors) and length(cursors) == MapSet.size(MapSet.new(cursors)) and
      Enum.all?(cursors, &(not is_binary(after_cursor) or &1 > after_cursor)) and
      case {entries, next} do
        {[], nil} ->
          true

        {[], _} ->
          false

        {_, nil} ->
          true

        {_, ^next} ->
          List.last(cursors) == next and (not is_binary(after_cursor) or next > after_cursor)
      end
  end

  defp directory_error("directory_full"), do: :directory_full
  defp directory_error("invalid_announcement"), do: :invalid_announcement
  defp directory_error("invalid_query"), do: :invalid_query
  defp directory_error("federation_unavailable"), do: :federation_unavailable
  defp directory_error("federation_busy"), do: :federation_busy
  defp directory_error(_), do: :relay_discovery_unavailable

  defp parse_pubkey(value) do
    value = String.trim(value)
    value = String.trim_leading(value, "0x")

    case Base.decode16(value, case: :mixed) do
      {:ok, <<pubkey::binary-size(32)>>} ->
        pubkey

      _ ->
        case Base.decode64(value) do
          {:ok, <<pubkey::binary-size(32)>>} -> pubkey
          _ -> nil
        end
    end
  end
end
