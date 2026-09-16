defmodule Arc.CLI.StatusProbe do
  @moduledoc """
  Bounded, unauthenticated relay greeting check. Never registers a citizen route.
  """

  alias Arc.Net.Handshake

  @timeout_ms 2_000

  def check({host, port}, pin) do
    task = Task.async(fn -> probe(host, port, pin) end)

    case Task.yield(task, @timeout_ms) || Task.shutdown(task, :brutal_kill) do
      {:ok, result} -> result
      _ -> %{"status" => "unreachable", "reason" => "timeout"}
    end
  end

  defp probe(host, port, pin) do
    case :gen_tcp.connect(host, port, [:binary, active: false], @timeout_ms) do
      {:ok, socket} ->
        try do
          read_hello(socket, pin)
        after
          :gen_tcp.close(socket)
        end

      {:error, reason} ->
        %{"status" => "unreachable", "reason" => reason_label(reason)}
    end
  end

  defp read_hello(socket, pin) do
    with {:ok, hello} <- :gen_tcp.recv(socket, 64, @timeout_ms),
         {:ok, public_key, _challenge} <- Handshake.decode_relay_hello(hello) do
      if pin != nil and pin != public_key do
        %{
          "status" => "key_mismatch",
          "reason" => "relay greeting does not match the configured pin"
        }
      else
        %{
          "status" => "reachable",
          "pin_matches" => if(pin, do: true, else: nil),
          "public_key" => Base.encode16(public_key, case: :lower)
        }
      end
    else
      {:error, reason} -> %{"status" => "unreachable", "reason" => reason_label(reason)}
    end
  end

  defp reason_label(:econnrefused), do: "connection refused"
  defp reason_label(:nxdomain), do: "host not found"
  defp reason_label(:timeout), do: "timeout"
  defp reason_label(:closed), do: "connection closed before relay greeting"
  defp reason_label(_), do: "relay greeting unavailable"
end
