defmodule Arc.Net.Load do
  @moduledoc """
  Relay load harness focused on repeatable connection and packet pressure tests.

  This module is intentionally separate from product runtime code.
  """

  alias Arc.Data.Packet
  alias Arc.Identity
  alias Arc.Net.Handshake
  alias Arc.Net.Telemetry

  @default_connections 1_000
  @default_packets 0
  @default_payload_bytes 64
  @default_port 7_331
  @default_timeout_ms 2_000

  @type load_mode :: :drop | :forward

  @doc """
  Run a relay load pass.

  Options:
  - `:host` binary or charlist (default: localhost)
  - `:port` integer (default: 7331)
  - `:connections` integer >= 0 (default: 1000)
  - `:packets` integer >= 0 (default: 0)
  - `:payload_bytes` integer >= 1 (default: 64)
  - `:mode` `:drop | :forward` (default: :drop)
  - `:timeout_ms` integer >= 1 (default: 2000)
  """
  @spec run(keyword()) :: {:ok, map()} | {:error, atom()}
  def run(opts \\ []) do
    with {:ok, host} <- normalize_host(Keyword.get(opts, :host, ~c"localhost")),
         {:ok, port} <- normalize_port(Keyword.get(opts, :port, @default_port)),
         {:ok, connections} <-
           normalize_nonneg(Keyword.get(opts, :connections, @default_connections)),
         {:ok, packets} <- normalize_nonneg(Keyword.get(opts, :packets, @default_packets)),
         {:ok, payload_bytes} <-
           normalize_positive(Keyword.get(opts, :payload_bytes, @default_payload_bytes)),
         {:ok, mode} <- normalize_mode(Keyword.get(opts, :mode, :drop)),
         {:ok, timeout_ms} <-
           normalize_positive(Keyword.get(opts, :timeout_ms, @default_timeout_ms)) do
      do_run(host, port, connections, packets, payload_bytes, mode, timeout_ms)
    end
  end

  defp do_run(host, port, connections, packets, payload_bytes, mode, timeout_ms) do
    connect_started_at = now_ms()
    {clients, connect_errors} = open_clients(host, port, connections, timeout_ms)
    connect_elapsed_ms = now_ms() - connect_started_at

    send_started_at = now_ms()

    {packets_sent, send_errors} =
      send_packets(clients, packets, payload_bytes, mode)

    send_elapsed_ms = now_ms() - send_started_at

    close_clients(clients)

    summary = %{
      host: to_string(host),
      port: port,
      mode: mode,
      connections_requested: connections,
      connections_opened: length(clients),
      connect_errors: connect_errors,
      packets_requested: packets,
      packets_sent: packets_sent,
      send_errors: send_errors,
      connect_elapsed_ms: connect_elapsed_ms,
      send_elapsed_ms: send_elapsed_ms,
      connect_rate_per_sec: rate(length(clients), connect_elapsed_ms),
      packet_rate_per_sec: rate(packets_sent, send_elapsed_ms)
    }

    Telemetry.execute([:load, :completed], %{count: 1}, summary)
    {:ok, summary}
  end

  defp open_clients(host, port, connections, timeout_ms) do
    if connections == 0 do
      {[], 0}
    else
      Enum.reduce(1..connections, {[], 0}, fn _, {clients, errors} ->
        id = Identity.generate()

        case :gen_tcp.connect(host, port, [:binary, packet: :raw, active: false], timeout_ms) do
          {:ok, socket} ->
            case recv_relay_hello(socket, timeout_ms) do
              {:ok, relay_pubkey, relay_challenge} ->
                with {:ok, client_hello, _client_pubkey} <-
                       Handshake.client_hello(id, relay_pubkey, relay_challenge),
                     :ok <- :gen_tcp.send(socket, client_hello) do
                  {[{socket, id} | clients], errors}
                else
                  _ ->
                    :gen_tcp.close(socket)
                    {clients, errors + 1}
                end

              {:error, _} ->
                :gen_tcp.close(socket)
                {clients, errors + 1}
            end

          {:error, _} ->
            {clients, errors + 1}
        end
      end)
      |> then(fn {clients, errors} -> {Enum.reverse(clients), errors} end)
    end
  end

  defp recv_relay_hello(socket, timeout_ms) do
    relay_hello_bytes = 64

    with {:ok, relay_hello} <- :gen_tcp.recv(socket, relay_hello_bytes, timeout_ms),
         {:ok, relay_pubkey, relay_challenge} <- Handshake.decode_relay_hello(relay_hello) do
      {:ok, relay_pubkey, relay_challenge}
    else
      {:error, reason} -> {:error, reason}
    end
  end

  defp send_packets(_clients, 0, _payload_bytes, _mode), do: {0, 0}
  defp send_packets([], _packets, _payload_bytes, _mode), do: {0, 0}

  defp send_packets(clients, packets, payload_bytes, mode) do
    client_count = length(clients)
    clients_tuple = List.to_tuple(clients)
    drop_target = :crypto.strong_rand_bytes(32)

    Enum.reduce(0..(packets - 1), {0, 0}, fn idx, {sent, errors} ->
      {src_socket, src_id} = elem(clients_tuple, rem(idx, client_count))

      dst_pk =
        case mode do
          :drop ->
            drop_target

          :forward ->
            if client_count > 1 do
              {_dst_socket, dst_id} = elem(clients_tuple, rem(idx + 1, client_count))
              dst_id.public_key
            else
              drop_target
            end
        end

      packet = make_packet(src_id, dst_pk, idx, payload_bytes)

      case :gen_tcp.send(src_socket, <<byte_size(packet)::32-big, packet::binary>>) do
        :ok -> {sent + 1, errors}
        {:error, _} -> {sent, errors + 1}
      end
    end)
  end

  defp make_packet(src_id, dst_pk, seq, payload_bytes) do
    session_id = :crypto.strong_rand_bytes(16)
    nonce = :crypto.strong_rand_bytes(12)
    ciphertext = :crypto.strong_rand_bytes(payload_bytes)
    Packet.encode(src_id, dst_pk, session_id, seq, nonce, ciphertext)
  end

  defp close_clients(clients) do
    Enum.each(clients, fn {socket, _id} ->
      :gen_tcp.close(socket)
    end)
  end

  defp rate(_count, elapsed_ms) when elapsed_ms <= 0, do: 0.0
  defp rate(count, elapsed_ms), do: Float.round(count * 1_000 / elapsed_ms, 2)

  defp now_ms, do: System.monotonic_time(:millisecond)

  defp normalize_host(host) when is_binary(host) and host != "",
    do: {:ok, String.to_charlist(host)}

  defp normalize_host(host) when is_list(host) and host != [], do: {:ok, host}
  defp normalize_host(_), do: {:error, :invalid_host}

  defp normalize_port(port) when is_integer(port) and port > 0 and port < 65_536, do: {:ok, port}
  defp normalize_port(_), do: {:error, :invalid_port}

  defp normalize_nonneg(value) when is_integer(value) and value >= 0, do: {:ok, value}
  defp normalize_nonneg(_), do: {:error, :invalid_value}

  defp normalize_positive(value) when is_integer(value) and value > 0, do: {:ok, value}
  defp normalize_positive(_), do: {:error, :invalid_value}

  defp normalize_mode(:drop), do: {:ok, :drop}
  defp normalize_mode(:forward), do: {:ok, :forward}
  defp normalize_mode("drop"), do: {:ok, :drop}
  defp normalize_mode("forward"), do: {:ok, :forward}
  defp normalize_mode(_), do: {:error, :invalid_mode}
end
