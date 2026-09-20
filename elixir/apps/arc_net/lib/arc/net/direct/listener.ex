defmodule Arc.Net.Direct.Listener do
  @moduledoc false
  use GenServer

  alias Arc.Net.Direct
  alias Arc.Net.Direct.Connection

  @max_bad_handshakes 4
  @max_owner_queue 16

  def start_link(owner, opts) do
    with {:ok, pid} <- GenServer.start(__MODULE__, {owner, opts}),
         {:ok, port} <- GenServer.call(pid, :port) do
      {:ok, pid, port}
    end
  end

  @impl true
  def init({owner, opts}) do
    with :ok <- Direct.validate_common(owner, opts),
         {:ok, ip} <- ip(Keyword.get(opts, :ip, {127, 0, 0, 1})),
         {:ok, port} <- port(Keyword.get(opts, :port, 0)),
         {:ok, socket} <-
           :gen_tcp.listen(port, Direct.tcp_options(ip) ++ [ip: ip, reuseaddr: true]),
         {:ok, actual_port} <- :inet.port(socket) do
      state = %{
        owner: owner,
        owner_ref: Process.monitor(owner),
        opts: opts,
        socket: socket,
        port: actual_port,
        failures: 0,
        connection: nil,
        accepted?: false
      }

      Process.send_after(self(), :expired, Direct.timeout(opts))
      send(self(), :accept)
      {:ok, state}
    else
      {:error, reason} -> {:stop, reason}
    end
  end

  @impl true
  def handle_call(:port, _from, state), do: {:reply, {:ok, state.port}, state}

  def handle_call(:arc_direct_close, _from, state) do
    if is_pid(state.connection), do: Connection.close(state.connection)
    send(state.owner, {:arc_direct_closed, state.opts[:tag], self(), :closed_by_owner})
    {:stop, :normal, :ok, %{state | connection: nil}}
  end

  def handle_call(
        {:arc_direct_send_packet, packet, deadline_ms},
        _from,
        %{connection: connection} = state
      )
      when is_pid(connection) do
    {:reply, Connection.send_packet(connection, packet, deadline_ms), state}
  end

  def handle_call({:arc_direct_send_packet, _packet, _deadline_ms}, _from, state),
    do: {:reply, {:error, :not_connected}, state}

  @impl true
  def handle_info(:accept, state) do
    listener = self()
    socket = state.socket
    timeout = Direct.timeout(state.opts)

    Task.start(fn ->
      case :gen_tcp.accept(socket, timeout) do
        {:ok, tcp} ->
          case :gen_tcp.controlling_process(tcp, listener) do
            :ok ->
              send(listener, {:accepted, tcp})

            {:error, reason} ->
              :gen_tcp.close(tcp)
              send(listener, {:accept_failed, reason})
          end

        {:error, reason} ->
          send(listener, {:accept_failed, reason})
      end
    end)

    {:noreply, state}
  end

  def handle_info({:accepted, tcp}, state) do
    case Direct.tls_server(tcp, state.opts) do
      {:ok, tcp} ->
        case Direct.accept(self(), tcp, state.opts) do
          {:ok, connection} ->
            _ = :gen_tcp.close(state.socket)
            # Keep the listener process as the connection's original
            # starter until its owner closes it. Exiting here can race OTP
            # socket cleanup on some platforms even after ownership moves.
            {:noreply, %{state | socket: nil, accepted?: true, connection: connection}}

          {:error, _reason} ->
            retry(state)
        end

      {:error, _reason} ->
        _ = :gen_tcp.close(tcp)
        retry(state)
    end
  end

  def handle_info({:accept_failed, :timeout}, state), do: retry(state)
  def handle_info({:accept_failed, _reason}, state), do: retry(state)

  def handle_info(:expired, %{accepted?: true} = state), do: {:noreply, state}
  def handle_info(:expired, state), do: {:stop, :listener_expired, state}

  def handle_info({:arc_direct_connected, tag, connection}, %{connection: connection} = state) do
    send(state.owner, {:arc_direct_connected, tag, self()})
    {:noreply, state}
  end

  def handle_info(
        {:arc_direct_packet, tag, connection, packet},
        %{connection: connection} = state
      ) do
    if owner_queue_available?(state.owner) do
      send(state.owner, {:arc_direct_packet, tag, self(), packet})
      {:noreply, state}
    else
      :ok = Connection.close(connection)
      send(state.owner, {:arc_direct_closed, tag, self(), :owner_overloaded})
      {:stop, :normal, %{state | connection: nil}}
    end
  end

  def handle_info(
        {:arc_direct_closed, tag, connection, reason},
        %{connection: connection} = state
      ) do
    send(state.owner, {:arc_direct_closed, tag, self(), reason})
    {:stop, :normal, %{state | connection: nil}}
  end

  def handle_info({:DOWN, ref, :process, _pid, _reason}, %{owner_ref: ref} = state),
    do: {:stop, :normal, state}

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, state) do
    if state[:socket], do: :gen_tcp.close(state.socket)
    if is_pid(state[:connection]), do: Direct.close(state.connection)
    :ok
  end

  @impl true
  def format_status(_opt, [_pdict, state]),
    do: [data: [{~c"State", %{state | opts: Direct.redact_opts(state.opts)}}]]

  defp retry(%{failures: failures} = state) when failures + 1 >= @max_bad_handshakes,
    do: {:stop, :too_many_bad_handshakes, state}

  defp retry(state) do
    send(self(), :accept)
    {:noreply, %{state | failures: state.failures + 1}}
  end

  defp owner_queue_available?(owner) do
    case Process.info(owner, :message_queue_len) do
      {:message_queue_len, count} when count < @max_owner_queue -> true
      _ -> false
    end
  end

  defp ip(ip) when is_tuple(ip) and tuple_size(ip) in [4, 8], do: {:ok, ip}
  defp ip(_), do: {:error, :invalid_ip}
  defp port(port) when is_integer(port) and port in 0..65_535, do: {:ok, port}
  defp port(_), do: {:error, :invalid_port}
end
