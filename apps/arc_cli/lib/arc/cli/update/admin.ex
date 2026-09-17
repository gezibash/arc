defmodule Arc.CLI.Update.Admin do
  @moduledoc false

  use GenServer

  @max_request_bytes 64 * 1024
  @request_timeout_ms 5_000

  def start_link(opts) when is_list(opts), do: GenServer.start_link(__MODULE__, opts)

  @impl true
  def init(opts) do
    Process.flag(:trap_exit, true)
    state_dir = opts |> Keyword.fetch!(:state_dir) |> Path.expand()
    socket_path = Path.join(state_dir, "admin.sock")

    with :ok <- File.mkdir_p(state_dir),
         :ok <- File.chmod(state_dir, 0o700),
         :ok <- require_absent(socket_path),
         {:ok, listener} <- open_listener(socket_path),
         {:ok, acceptor} <-
           Task.start_link(fn -> accept_loop(listener, Keyword.fetch!(opts, :manager)) end) do
      {:ok, %{listener: listener, socket_path: socket_path, acceptor: acceptor}}
    else
      {:error, reason} -> {:stop, {:admin_socket, reason}}
    end
  end

  @impl true
  def handle_info({:EXIT, pid, reason}, %{acceptor: pid} = state) do
    {:stop, {:acceptor_exit, reason}, state}
  end

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, state) do
    _ = :socket.close(state.listener)
    if Process.alive?(state.acceptor), do: Process.exit(state.acceptor, :shutdown)
    _ = File.rm(state.socket_path)
    :ok
  end

  defp require_absent(path) do
    case File.lstat(path) do
      {:error, :enoent} -> :ok
      {:ok, _} -> {:error, :socket_path_exists}
      {:error, reason} -> {:error, reason}
    end
  end

  defp open_listener(path) do
    with {:ok, listener} <- :socket.open(:local, :stream, :default),
         :ok <- :socket.bind(listener, %{family: :local, path: String.to_charlist(path)}),
         :ok <- :socket.listen(listener),
         :ok <- File.chmod(path, 0o600) do
      {:ok, listener}
    end
  end

  defp accept_loop(listener, manager) do
    case :socket.accept(listener) do
      {:ok, socket} ->
        # One local request is processed before accepting another. The bounded
        # request deadline also prevents a client from holding this endpoint.
        handle_socket(socket, manager)
        accept_loop(listener, manager)

      {:error, _reason} ->
        :ok
    end
  end

  defp handle_socket(socket, manager) do
    deadline = System.monotonic_time(:millisecond) + @request_timeout_ms

    response =
      with {:ok, request} <- recv_request(socket, deadline, ""),
           {:ok, op, params} <- valid_request(request),
           {:ok, result} <- manager_request(manager, op, params, deadline) do
        %{"ok" => true, "result" => result}
      else
        {:error, reason} -> %{"ok" => false, "error" => error_message(reason)}
      end

    _ = send_response(socket, response, deadline)
    _ = :socket.close(socket)
  end

  defp recv_request(_socket, _deadline, buffer) when byte_size(buffer) > @max_request_bytes,
    do: {:error, :request_too_large}

  defp recv_request(socket, deadline, buffer) do
    case String.split(buffer, "\n", parts: 2) do
      [line, _rest] when byte_size(line) <= @max_request_bytes and byte_size(line) > 0 ->
        try do
          {:ok, :json.decode(line)}
        rescue
          _ -> {:error, :invalid_json}
        end

      [_line, _rest] ->
        {:error, :request_too_large}

      _ ->
        remaining = deadline - System.monotonic_time(:millisecond)

        if remaining <= 0 do
          {:error, :timeout}
        else
          case :socket.recv(socket, 0, remaining) do
            {:ok, data} when is_binary(data) -> recv_request(socket, deadline, buffer <> data)
            {:error, reason} -> {:error, reason}
          end
        end
    end
  end

  defp valid_request(%{"op" => op, "params" => params})
       when op in ["status", "check", "apply"] and is_map(params),
       do: {:ok, op, params}

  defp valid_request(_), do: {:error, :invalid_request}

  defp manager_request(manager, op, params, deadline) do
    remaining = deadline - System.monotonic_time(:millisecond)

    if remaining <= 0 do
      {:error, :timeout}
    else
      # This task is deliberately unlinked from the socket acceptor. A faulty
      # manager response becomes one failed request rather than ending service
      # administration. The serial accept loop permits at most one at a time.
      ref = make_ref()
      owner = self()

      {:ok, pid} =
        Task.start(fn ->
          result =
            try do
              manager.request(op, params)
            rescue
              _exception -> {:error, :manager_unavailable}
            catch
              :exit, _reason -> {:error, :manager_unavailable}
            end

          send(owner, {ref, result})
        end)

      receive do
        {^ref, {:ok, result}} when is_map(result) -> {:ok, result}
        {^ref, {:error, reason}} -> {:error, reason}
        {^ref, _} -> {:error, :invalid_manager_response}
      after
        remaining ->
          Process.exit(pid, :kill)
          {:error, :timeout}
      end
    end
  end

  defp send_response(socket, response, deadline) do
    case deadline - System.monotonic_time(:millisecond) do
      remaining when remaining > 0 ->
        :socket.send(socket, [IO.iodata_to_binary(:json.encode(response)), "\n"], remaining)

      _ ->
        {:error, :timeout}
    end
  end

  defp error_message(reason) when is_atom(reason), do: Atom.to_string(reason)
  defp error_message(_reason), do: "request_failed"
end
