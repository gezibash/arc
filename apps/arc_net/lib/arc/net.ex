defmodule Arc.Net do
  @moduledoc """
  Public API for Arc network transport.

  Provides relay-mesh delivery as a tier between in-process AgentRegistry
  and file-based mailbox fallback.
  """

  alias Arc.Data.Packet
  alias Arc.Net.TransportManager

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
