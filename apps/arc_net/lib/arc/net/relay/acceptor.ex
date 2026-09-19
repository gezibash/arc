defmodule Arc.Net.Relay.Acceptor do
  @moduledoc false

  alias Arc.Net.Connection

  @doc false
  def accept_loop(listen_socket, relay_pid, relay_public_key) do
    case :gen_tcp.accept(listen_socket) do
      {:ok, socket} ->
        # Not linked: a crash in one connection must not stop this acceptor and the other
        # connections. A connection closes when its relay stops, because it monitors the relay.
        {:ok, conn} =
          Connection.start(
            socket: socket,
            role: :relay_client,
            relay_pid: relay_pid,
            relay_pubkey: relay_public_key
          )

        :ok = :gen_tcp.controlling_process(socket, conn)
        Connection.send_relay_hello(conn)
        Connection.activate(conn)
        accept_loop(listen_socket, relay_pid, relay_public_key)

      {:error, :closed} ->
        :ok

      {:error, _reason} ->
        accept_loop(listen_socket, relay_pid, relay_public_key)
    end
  end
end
