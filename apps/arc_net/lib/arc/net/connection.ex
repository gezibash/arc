defmodule Arc.Net.Connection do
  @moduledoc """
  GenServer managing a single TCP socket.

  Two roles:
    - `:client`       — outbound connection from Transport to a relay.
                        All received frames are forwarded to Transport.
    - `:relay_client` — inbound connection accepted by Relay.
                        First 96 bytes = client pubkey + relay challenge signature.
                        Signature must verify before route registration.
                        Subsequent framed packets → routed by Relay.

  Wire framing: <<4-byte big-endian length>><packet binary>

  The socket starts passive ({active: false}). The caller must transfer
  controlling process ownership and then call activate/1 to begin receiving.

  Frame cap: `Application.get_env(:arc_net, :max_frame_bytes)` bounds the
  largest frame a connection accepts. It is `:unbounded` by default. The
  32-bit length header is the hard ceiling. An operator lowers the cap with
  `--max-frame-bytes` or `ARC_RELAY_MAX_FRAME_BYTES`. A frame whose header
  advertises more than the cap closes the connection with `frame_too_large`.

  A relay advertises its cap in a relay info frame right after the hello
  (see `Arc.Net.Handshake`). A `:client` connection records it and refuses
  `send_packet/2` for a packet over that cap with `{:error, :frame_too_large}`
  instead of losing the connection.
  """

  use GenServer

  alias Arc.Net.Handshake
  alias Arc.Net.Relay
  alias Arc.Net.Telemetry
  alias Arc.Net.Transport

  @default_max_frame_bytes :unbounded
  @default_hello_timeout_ms 5_000
  @default_max_mailbox_len 2_048
  @default_overflow_policy :disconnect

  # --- Client API ---

  def start_link(opts) do
    GenServer.start_link(__MODULE__, opts)
  end

  @doc """
  Frame and write a packet to the socket.
  Returns `{:error, :frame_too_large}` without sending when the relay
  advertised a frame cap smaller than the packet.
  """
  def send_packet(conn_pid, packet) when is_binary(packet) do
    GenServer.call(conn_pid, {:send_packet, packet})
  end

  @doc "The frame cap the relay advertised, or `:unbounded`. `nil` before the relay info arrives."
  def peer_max_frame_bytes(conn_pid) do
    GenServer.call(conn_pid, :peer_max_frame_bytes)
  end

  @doc "Best-effort packet forward path used by relay fanout."
  def forward_packet(conn_pid, packet) when is_binary(packet) do
    {max_mailbox_len, overflow_policy} = backpressure_settings()
    queue_len = message_queue_len(conn_pid)

    if overflow?(queue_len, max_mailbox_len) do
      handle_overflow(conn_pid, packet, queue_len, overflow_policy)
    else
      GenServer.cast(conn_pid, {:forward_packet, packet})
      :ok
    end
  end

  @doc "Arm {active: :once}. Call after controlling_process transfer."
  def activate(conn_pid) do
    GenServer.cast(conn_pid, :activate)
  end

  @doc "Send relay identity hello (32-byte relay pubkey) on new relay connections."
  def send_relay_hello(conn_pid) do
    GenServer.cast(conn_pid, :send_relay_hello)
  end

  @doc "Close the socket and stop the connection process."
  def close(conn_pid) do
    GenServer.cast(conn_pid, :close)
  end

  # --- GenServer Callbacks ---

  @impl GenServer
  def init(opts) do
    socket = Keyword.fetch!(opts, :socket)
    role = Keyword.fetch!(opts, :role)
    max_frame_bytes = max_frame_bytes_setting()
    hello_timeout_ms = Application.get_env(:arc_net, :hello_timeout_ms, @default_hello_timeout_ms)

    parent =
      case role do
        :client -> Keyword.fetch!(opts, :transport_pid)
        :relay_client -> Keyword.fetch!(opts, :relay_pid)
      end

    hello_timer_ref =
      if role == :relay_client do
        Process.send_after(self(), :hello_timeout, hello_timeout_ms)
      else
        nil
      end

    state = %{
      socket: socket,
      role: role,
      parent: parent,
      relay_pubkey: Keyword.get(opts, :relay_pubkey),
      relay_challenge: Keyword.get(opts, :relay_challenge, maybe_relay_challenge(role)),
      recv_buffer: <<>>,
      # relay_client waits for signed hello first; client is always in frame mode
      hello_done: role == :client,
      hello_timer_ref: hello_timer_ref,
      max_frame_bytes: max_frame_bytes,
      peer_max_frame_bytes: nil
    }

    emit([:connection, :started], %{count: 1}, %{role: role})
    {:ok, state}
  end

  @impl GenServer
  def handle_cast(:activate, state) do
    case :inet.setopts(state.socket, active: :once) do
      :ok ->
        {:noreply, state}

      {:error, reason} ->
        emit([:connection, :closed], %{count: 1}, %{reason: {:activate_failed, reason}})
        {:stop, :normal, state}
    end
  end

  def handle_cast(
        :send_relay_hello,
        %{role: :relay_client, relay_pubkey: relay_pubkey, relay_challenge: relay_challenge} =
          state
      ) do
    case Handshake.relay_hello(relay_pubkey, relay_challenge) do
      {:ok, hello} ->
        info = Handshake.relay_info(state.max_frame_bytes)

        with :ok <- :gen_tcp.send(state.socket, hello),
             :ok <- send_framed(state.socket, info) do
          {:noreply, state}
        else
          {:error, reason} ->
            emit([:connection, :closed], %{count: 1}, %{reason: {:relay_hello_failed, reason}})
            {:stop, :normal, state}
        end

      {:error, reason} ->
        emit([:connection, :closed], %{count: 1}, %{reason: reason})
        {:stop, :normal, state}
    end
  end

  def handle_cast(:send_relay_hello, state) do
    {:noreply, state}
  end

  def handle_cast(:close, state) do
    :gen_tcp.close(state.socket)
    emit([:connection, :closed], %{count: 1}, %{reason: :closed_by_owner})
    {:stop, :normal, state}
  end

  def handle_cast({:forward_packet, packet}, state) do
    case send_framed(state.socket, packet) do
      :ok ->
        {:noreply, state}

      {:error, reason} ->
        emit([:connection, :send_error], %{count: 1, bytes: byte_size(packet)}, %{reason: reason})
        {:stop, :normal, state}
    end
  end

  @impl GenServer
  def handle_call({:send_packet, packet}, _from, %{peer_max_frame_bytes: cap} = state)
      when is_integer(cap) and byte_size(packet) > cap do
    emit([:connection, :send_error], %{count: 1, bytes: byte_size(packet)}, %{
      reason: :frame_too_large,
      peer_max_frame_bytes: cap
    })

    {:reply, {:error, :frame_too_large}, state}
  end

  def handle_call({:send_packet, packet}, _from, state) do
    case send_framed(state.socket, packet) do
      :ok ->
        {:reply, :ok, state}

      {:error, _} = error ->
        emit([:connection, :send_error], %{count: 1, bytes: byte_size(packet)}, %{reason: error})
        {:stop, :normal, error, state}
    end
  end

  def handle_call(:peer_max_frame_bytes, _from, state) do
    {:reply, state.peer_max_frame_bytes, state}
  end

  def handle_call(_msg, _from, state) do
    {:reply, {:error, :unsupported}, state}
  end

  @impl GenServer
  def handle_info(:hello_timeout, %{hello_done: false} = state) do
    :gen_tcp.close(state.socket)
    emit([:connection, :closed], %{count: 1}, %{reason: :hello_timeout})
    {:stop, :normal, state}
  end

  def handle_info(:hello_timeout, state) do
    {:noreply, state}
  end

  def handle_info({:tcp, socket, data}, %{socket: socket} = state) do
    state = %{state | recv_buffer: state.recv_buffer <> data}

    case process_buffer(state) do
      {:ok, state} ->
        case :inet.setopts(socket, active: :once) do
          :ok ->
            {:noreply, state}

          {:error, reason} ->
            emit([:connection, :closed], %{count: 1}, %{reason: {:reactivate_failed, reason}})
            {:stop, :normal, state}
        end

      {:error, reason, state} ->
        :gen_tcp.close(socket)
        emit([:connection, :closed], %{count: 1}, %{reason: reason})
        {:stop, :normal, state}
    end
  end

  def handle_info({:tcp_closed, socket}, %{socket: socket} = state) do
    emit([:connection, :closed], %{count: 1}, %{reason: :tcp_closed})
    {:stop, :normal, state}
  end

  def handle_info({:tcp_error, socket, reason}, %{socket: socket} = state) do
    emit([:connection, :closed], %{count: 1}, %{reason: {:tcp_error, reason}})
    {:stop, :normal, state}
  end

  def handle_info(_msg, state), do: {:noreply, state}

  # --- Private ---

  defp process_buffer(%{hello_done: false} = state) do
    case Handshake.extract_client_hello(state.recv_buffer) do
      {:ok, pubkey, signature, rest} ->
        if Handshake.verify_client_hello(
             pubkey,
             signature,
             state.relay_pubkey,
             state.relay_challenge
           ) do
          cancel_hello_timer(state.hello_timer_ref)
          Relay.register(state.parent, self(), pubkey)
          process_frames(%{state | recv_buffer: rest, hello_done: true, hello_timer_ref: nil})
        else
          {:error, :invalid_hello_signature, state}
        end

      :more ->
        {:ok, state}

      {:error, _} ->
        {:error, :invalid_client_hello, state}
    end
  end

  defp process_buffer(state), do: process_frames(state)

  defp process_frames(state) do
    case drain_frames(state.recv_buffer, state.max_frame_bytes) do
      {:ok, frames, rest} ->
        state = Enum.reduce(frames, state, &dispatch_frame/2)
        {:ok, %{state | recv_buffer: rest}}

      {:error, reason} ->
        {:error, reason, state}
    end
  end

  defp drain_frames(buffer, max_frame_bytes), do: drain_frames(buffer, max_frame_bytes, [])

  defp drain_frames(buffer, _max_frame_bytes, acc) when byte_size(buffer) < 4 do
    {:ok, Enum.reverse(acc), buffer}
  end

  defp drain_frames(<<len::32-big, _::binary>>, max_frame_bytes, _acc)
       when is_integer(max_frame_bytes) and len > max_frame_bytes do
    {:error, :frame_too_large}
  end

  defp drain_frames(<<len::32-big, rest::binary>> = buffer, _max_frame_bytes, acc)
       when byte_size(rest) < len do
    {:ok, Enum.reverse(acc), buffer}
  end

  defp drain_frames(<<len::32-big, packet::binary-size(len), rest::binary>>, max_frame_bytes, acc) do
    drain_frames(rest, max_frame_bytes, [packet | acc])
  end

  defp dispatch_frame(packet, %{role: :client, parent: transport_pid} = state) do
    case Handshake.decode_relay_info(packet) do
      {:ok, cap} ->
        %{state | peer_max_frame_bytes: cap}

      :error ->
        Transport.packet_received(transport_pid, packet)
        state
    end
  end

  defp dispatch_frame(packet, %{role: :relay_client, parent: relay_pid} = state) do
    Relay.route_packet(relay_pid, self(), packet)
    state
  end

  defp send_framed(socket, packet) do
    len = byte_size(packet)
    :gen_tcp.send(socket, <<len::32-big, packet::binary>>)
  end

  defp cancel_hello_timer(nil), do: :ok

  defp cancel_hello_timer(ref) do
    Process.cancel_timer(ref, async: true, info: false)
    :ok
  end

  defp maybe_relay_challenge(:relay_client), do: :crypto.strong_rand_bytes(32)
  defp maybe_relay_challenge(_), do: nil

  # `:unbounded`, `nil`, or `0` mean no cap. A positive integer is the cap in bytes.
  defp max_frame_bytes_setting do
    case Application.get_env(:arc_net, :max_frame_bytes, @default_max_frame_bytes) do
      value when is_integer(value) and value > 0 -> value
      _ -> :unbounded
    end
  end

  defp backpressure_settings do
    max_mailbox_len =
      case Application.get_env(:arc_net, :connection_max_mailbox_len, @default_max_mailbox_len) do
        :infinity -> :infinity
        value when is_integer(value) and value >= 0 -> value
        _ -> @default_max_mailbox_len
      end

    overflow_policy =
      case Application.get_env(:arc_net, :connection_overflow_policy, @default_overflow_policy) do
        :drop -> :drop
        _ -> :disconnect
      end

    {max_mailbox_len, overflow_policy}
  end

  defp message_queue_len(conn_pid) do
    case Process.info(conn_pid, :message_queue_len) do
      {:message_queue_len, len} when is_integer(len) and len >= 0 -> len
      _ -> 0
    end
  end

  defp overflow?(_queue_len, :infinity), do: false
  defp overflow?(queue_len, max_mailbox_len), do: queue_len >= max_mailbox_len

  defp handle_overflow(_conn_pid, packet, queue_len, :drop) do
    emit(
      [:connection, :backpressure, :drop],
      %{count: 1, bytes: byte_size(packet)},
      %{queue_len: queue_len}
    )

    {:error, :backpressure}
  end

  defp handle_overflow(conn_pid, packet, queue_len, :disconnect) do
    if Process.alive?(conn_pid) do
      close(conn_pid)
    end

    emit(
      [:connection, :backpressure, :disconnect],
      %{count: 1, bytes: byte_size(packet)},
      %{queue_len: queue_len}
    )

    {:error, :backpressure}
  end

  defp emit(event_suffix, measurements, metadata) do
    Telemetry.execute(event_suffix, measurements, metadata)
  end
end
