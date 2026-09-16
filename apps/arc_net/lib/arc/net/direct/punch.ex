defmodule Arc.Net.Direct.Punch do
  @moduledoc false
  use GenServer

  alias Arc.Net.Direct

  @max_bad_candidates 4
  @max_attempts 2

  def run(owner, opts) do
    with {:ok, pid} <- GenServer.start(__MODULE__, {owner, opts, self()}) do
      await(pid, Direct.timeout(opts))
    end
  end

  def validate(opts, host, port) do
    with :ok <- local_ip(Keyword.get(opts, :local_ip)),
         :ok <- local_port(Keyword.get(opts, :local_port)),
         :ok <- role(Keyword.get(opts, :role)),
         :ok <- same_family(Keyword.fetch!(opts, :local_ip), host),
         :ok <- different_endpoint(opts, host, port),
         true <- is_integer(port) or {:error, :invalid_port} do
      :ok
    else
      {:error, _} = error -> error
    end
  end

  @impl true
  def init({owner, opts, caller}) do
    Process.flag(:trap_exit, true)
    host = Keyword.fetch!(opts, :host)
    port = Keyword.fetch!(opts, :port)
    local_ip = Keyword.fetch!(opts, :local_ip)
    local_port = Keyword.fetch!(opts, :local_port)
    deadline = System.monotonic_time(:millisecond) + Direct.timeout(opts)
    punch_opts = Keyword.put(opts, :punch_owner, owner)

    case listen(local_ip, local_port) do
      {:ok, listener} ->
        acceptor = start_acceptor(listener, self())
        dialer = start_dialer(self(), punch_opts, host, port, deadline)

        state = %{
          owner: owner,
          owner_ref: Process.monitor(owner),
          caller_ref: monitor_caller(caller, owner),
          opts: punch_opts,
          host: host,
          port: port,
          listener: listener,
          deadline: deadline,
          waiters: [],
          workers: MapSet.new([acceptor, dialer]),
          bad_candidates: 0,
          attempts: 1,
          connection: nil,
          handed_off: false,
          result: nil
        }

        Process.send_after(self(), :expired, Direct.timeout(opts))
        {:ok, state}

      {:error, reason} ->
        {:stop, reason}
    end
  end

  @impl true
  def handle_call(:await, from, state) do
    case state.result do
      nil -> {:noreply, %{state | waiters: [from | state.waiters]}}
      result -> {:stop, :normal, result, %{state | handed_off: match?({:ok, _}, result)}}
    end
  end

  def handle_call(:arc_direct_close, _from, state),
    do: {:stop, :normal, :ok, finish(state, {:error, :closed_by_owner})}

  @impl true
  def handle_info({:accepted, worker, tcp}, state) do
    state = %{state | workers: MapSet.delete(state.workers, worker)}

    if peer?(tcp, state.host, state.port) do
      candidate = start_candidate(self(), state.owner, tcp, state.opts, state.deadline)
      {:noreply, %{state | workers: MapSet.put(state.workers, candidate)}}
    else
      :gen_tcp.close(tcp)
      retry_after(state, :accept)
    end
  end

  def handle_info({:candidate, worker, {:ok, connection}}, state) do
    state = %{state | workers: MapSet.delete(state.workers, worker)}

    if state.result || remaining(state.deadline) <= 0 do
      send(worker, {:punch_reject, connection})
      {:noreply, state}
    else
      send(worker, {:punch_keep, connection})
      succeed(%{state | connection: connection}, connection)
    end
  end

  def handle_info({:candidate, worker, source, {:error, _reason}}, state) do
    state = %{state | workers: MapSet.delete(state.workers, worker)}
    retry_after(state, source)
  end

  def handle_info(:expired, state),
    do: {:stop, :normal, finish(state, {:error, :punch_timeout})}

  def handle_info({:accept_error, worker, _reason}, state),
    do: {:noreply, %{state | workers: MapSet.delete(state.workers, worker)}}

  def handle_info({:DOWN, ref, :process, _pid, _reason}, %{owner_ref: ref} = state),
    do: {:stop, :normal, finish(state, {:error, :owner_down})}

  def handle_info({:DOWN, ref, :process, _pid, _reason}, %{caller_ref: ref} = state),
    do: {:stop, :normal, finish(state, {:error, :caller_down})}

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, state) do
    if state[:listener], do: :gen_tcp.close(state.listener)
    Enum.each(state[:workers] || [], &send(&1, :punch_cancel))
    if state.connection && not state.handed_off, do: Direct.close(state.connection)
    reply_waiters(state[:waiters] || [], state[:result] || {:error, :punch_closed})
    :ok
  end

  @impl true
  def format_status(_opt, _status), do: [data: [{~c"State", :redacted}]]

  defp await(pid, timeout) do
    GenServer.call(pid, :await, timeout + 500)
  catch
    :exit, _ ->
      Direct.close(pid)
      {:error, :punch_timeout}
  end

  defp listen(ip, port),
    do:
      :gen_tcp.listen(port, Direct.tcp_options(ip) ++ [ip: ip, reuseaddr: true, reuseport: true])

  defp start_acceptor(listener, punch), do: spawn(fn -> accept(listener, punch) end)

  defp accept(listener, punch) do
    case :gen_tcp.accept(listener, 250) do
      {:ok, tcp} ->
        :ok = :gen_tcp.controlling_process(tcp, punch)
        send(punch, {:accepted, self(), tcp})

      {:error, :timeout} ->
        accept(listener, punch)

      {:error, reason} ->
        send(punch, {:accept_error, self(), reason})
    end
  end

  defp start_dialer(punch, opts, host, port, deadline),
    do: spawn(fn -> dial(punch, opts, host, port, deadline) end)

  defp dial(punch, opts, host, port, deadline) do
    local_ip = Keyword.fetch!(opts, :local_ip)
    local_port = Keyword.fetch!(opts, :local_port)

    result =
      case :gen_tcp.connect(
             host,
             port,
             Direct.tcp_options(host) ++
               [ip: local_ip, port: local_port, reuseaddr: true, reuseport: true],
             remaining(deadline)
           ) do
        {:ok, tcp} -> authenticate(punch, tcp, opts, deadline, Keyword.fetch!(opts, :punch_owner))
        {:error, reason} -> {:error, reason}
      end

    report(punch, :dial, result)
  end

  defp start_candidate(punch, owner, tcp, opts, deadline) do
    worker =
      spawn(fn ->
        receive do
          {:tcp, socket} ->
            result = authenticate(punch, socket, opts, deadline, owner)
            report(punch, :accept, result)
        end
      end)

    :ok = :gen_tcp.controlling_process(tcp, worker)
    send(worker, {:tcp, tcp})
    worker
  end

  defp authenticate(_punch, tcp, opts, deadline, owner) do
    role = Keyword.fetch!(opts, :role)

    with {:ok, ssl} <- tls(tcp, opts, role, remaining(deadline)),
         timeout <- remaining(deadline),
         true <- timeout > 0 or {:error, :timeout},
         bounded_opts = Keyword.put(opts, :timeout_ms, timeout),
         {:ok, connection} <- Direct.start_connection(owner, ssl, bounded_opts, role) do
      {:ok, connection}
    else
      {:error, _} = error ->
        :gen_tcp.close(tcp)
        error
    end
  end

  defp tls(tcp, opts, :client, timeout),
    do: :ssl.connect(tcp, Direct.tls_options(opts, :client), timeout)

  defp tls(tcp, opts, :server, timeout),
    do: Direct.tls_server(tcp, Keyword.put(opts, :timeout_ms, timeout))

  defp report(punch, _source, {:ok, connection}) do
    ref = Process.monitor(punch)
    send(punch, {:candidate, self(), {:ok, connection}})

    receive do
      {:punch_keep, ^connection} -> :ok
      {:punch_reject, ^connection} -> Direct.close(connection)
      :punch_cancel -> Direct.close(connection)
      {:DOWN, ^ref, :process, ^punch, _reason} -> Direct.close(connection)
    end
  end

  defp report(punch, source, error), do: send(punch, {:candidate, self(), source, error})

  defp retry_after(%{bad_candidates: count} = state, :accept)
       when count + 1 >= @max_bad_candidates,
       do: {:stop, :normal, finish(state, {:error, :too_many_bad_candidates})}

  defp retry_after(state, :accept) do
    state = %{state | bad_candidates: state.bad_candidates + 1}
    {:noreply, rearm_acceptor(state)}
  end

  defp retry_after(state, :dial), do: retry_dial(state)

  defp retry_dial(%{attempts: attempts} = state) when attempts >= @max_attempts,
    do: {:noreply, state}

  defp retry_dial(state) do
    worker = start_dialer(self(), state.opts, state.host, state.port, state.deadline)
    workers = MapSet.put(state.workers, worker)
    {:noreply, %{state | attempts: state.attempts + 1, workers: workers}}
  end

  defp rearm_acceptor(state) do
    worker = start_acceptor(state.listener, self())
    %{state | workers: MapSet.put(state.workers, worker)}
  end

  defp finish(state, result) do
    reply_waiters(state.waiters, result)
    %{state | result: result, waiters: []}
  end

  defp succeed(%{waiters: []} = state, connection),
    do: {:noreply, %{state | result: {:ok, connection}}}

  defp succeed(state, connection),
    do: {:stop, :normal, finish(%{state | handed_off: true}, {:ok, connection})}

  defp reply_waiters(waiters, result), do: Enum.each(waiters, &GenServer.reply(&1, result))

  defp peer?(tcp, host, port) do
    case :inet.peername(tcp) do
      {:ok, {^host, ^port}} -> true
      _ -> false
    end
  end

  defp remaining(deadline), do: max(deadline - System.monotonic_time(:millisecond), 0)
  defp local_ip(ip) when is_tuple(ip) and tuple_size(ip) in [4, 8], do: :ok
  defp local_ip(_), do: {:error, :invalid_local_ip}
  defp local_port(port) when is_integer(port) and port in 1..65_535, do: :ok
  defp local_port(_), do: {:error, :invalid_local_port}
  defp role(role) when role in [:client, :server], do: :ok
  defp role(_), do: {:error, :invalid_role}
  defp same_family(left, right) when tuple_size(left) == tuple_size(right), do: :ok
  defp same_family(_, _), do: {:error, :address_family_mismatch}
  defp monitor_caller(caller, owner) when caller == owner, do: nil
  defp monitor_caller(caller, _owner), do: Process.monitor(caller)

  defp different_endpoint(opts, host, port) do
    if Keyword.fetch!(opts, :local_ip) == host and Keyword.fetch!(opts, :local_port) == port,
      do: {:error, :self_endpoint},
      else: :ok
  end
end
