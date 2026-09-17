defmodule Arc.CLI.Update.Manager do
  @moduledoc """
  Serializes operator requests for one managed service. Checks never apply code.
  Interrupted mutations remain blocked; startup never retries them.
  """
  use GenServer
  require Logger

  alias Arc.CLI.Update.Engine
  alias Arc.CLI.Update.Store

  def start_link(opts) do
    GenServer.start_link(__MODULE__, Keyword.fetch!(opts, :config), name: __MODULE__)
  end

  def request(op, params), do: GenServer.call(__MODULE__, {:request, op, params}, 4_000)

  @impl true
  def init(config) do
    Process.flag(:trap_exit, true)
    root = config.update.state_dir

    with :ok <- File.mkdir_p(root),
         :ok <- File.chmod(root, 0o700),
         {:ok, journal} <- Store.read(root, "journal") do
      state = %{
        config: config,
        root: root,
        job: nil,
        status: initial_status(journal)
      }

      Process.send_after(self(), :channel_check, 1_000)
      {:ok, state}
    else
      _ -> {:stop, :invalid_update_state}
    end
  end

  @impl true
  def handle_call({:request, "status", params}, _from, state) when params == %{} do
    {:reply, {:ok, public_status(state)}, state}
  end

  def handle_call({:request, op, params}, _from, state)
      when op in ["check", "apply"] and params == %{} do
    cond do
      state.job != nil ->
        {:reply, {:error, :update_busy}, state}

      state.status["reconciliation_required"] == true ->
        {:reply, {:error, :reconciliation_required}, state}

      true ->
        begin_job(op, state)
    end
  end

  def handle_call({:request, _, _}, _from, state) do
    {:reply, {:error, :invalid_update_request}, state}
  end

  @impl true
  def handle_info(:channel_check, state) do
    Process.send_after(self(), :channel_check, 3_600_000)

    if state.job == nil and state.status["reconciliation_required"] != true do
      {:reply, _result, next} = begin_job("check", state)
      {:noreply, next}
    else
      {:noreply, state}
    end
  end

  def handle_info({:update_result, pid, result}, %{job: {pid, ref}} = state) do
    Process.demonitor(ref, [:flush])
    status = result_status(result, state.root)

    if status["state"] in ["available", "restart_required"] do
      Logger.notice(
        "ARC update #{status["state"]}; inspect arc update status on the local service socket"
      )
    end

    {:noreply, %{state | job: nil, status: status}}
  end

  def handle_info({:DOWN, ref, :process, pid, _}, %{job: {pid, ref}} = state) do
    status = save_result(blocked("update_worker_failed", true), state.root)
    {:noreply, %{state | job: nil, status: status}}
  end

  def handle_info(_, state), do: {:noreply, state}

  defp begin_job(op, state) do
    status = %{"state" => if(op == "apply", do: "staging", else: "checking")}

    case Store.write(state.root, "journal", status) do
      :ok ->
        owner = self()

        job =
          :erlang.spawn_opt(
            fn ->
              result = Engine.run(op, state.config)
              send(owner, {:update_result, self(), result})
            end,
            [:link, :monitor]
          )

        next = %{state | job: job, status: status}
        {:reply, {:ok, public_status(next)}, next}

      _ ->
        {:reply, {:error, :update_state_write_failed}, state}
    end
  end

  defp initial_status(%{"state" => phase})
       when phase in ["staging", "applying", "observing", "committing"] do
    blocked("interrupted_update", true)
  end

  defp initial_status(%{"reconciliation_required" => true} = status),
    do: public_journal_status(status)

  defp initial_status(_), do: %{"state" => "idle"}

  @impl true
  def terminate(_reason, %{job: {pid, _}}) do
    Process.exit(pid, :kill)
    :ok
  end

  def terminate(_reason, _state), do: :ok

  defp result_status({:ok, status}, root), do: save_result(status, root)

  defp result_status({:error, reason}, root) do
    interrupted =
      case Store.read(root, "journal") do
        {:ok, %{"state" => phase}} when phase in ["applying", "observing", "committing"] -> true
        {:ok, _} -> false
        {:error, _} -> true
      end

    save_result(blocked(reason_string(reason), interrupted), root)
  end

  defp save_result(%{"reconciliation_required" => true} = status, root) do
    with {:ok, journal} <- Store.read(root, "journal"),
         persisted <- preserve_recovery_evidence(journal, status),
         :ok <- Store.write(root, "journal", persisted) do
      status
    else
      _ -> blocked("update_state_write_failed", true)
    end
  end

  defp save_result(status, root) do
    case Store.write(root, "journal", status) do
      :ok -> status
      _ -> blocked("update_state_write_failed", true)
    end
  end

  # The durable journal is the operator's evidence for a failed mutation. Keep
  # its release identity and last phase on disk while returning only a safe,
  # concise status through the local administrative interface.
  defp preserve_recovery_evidence(journal, status) do
    journal
    |> Map.merge(status)
    |> Map.put("interrupted_phase", Map.get(journal, "state", "unknown"))
  end

  defp public_journal_status(status) do
    Map.take(status, ["state", "reason", "reconciliation_required"])
  end

  defp blocked(reason, reconcile) do
    %{"state" => "blocked", "reason" => reason, "reconciliation_required" => reconcile}
  end

  # Error details can contain service state. Only controlled error codes leave
  # the worker through the administrative interface.
  defp reason_string(reason) when is_atom(reason), do: Atom.to_string(reason)

  defp reason_string(reason) when is_tuple(reason) and tuple_size(reason) > 0 do
    case elem(reason, 0) do
      code when is_atom(code) -> Atom.to_string(code)
      _ -> "update_failed"
    end
  end

  defp reason_string(_), do: "update_failed"

  defp public_status(state) do
    Map.merge(state.status, %{
      "service" => "relay",
      "channel" => state.config.update.channel || "stable",
      "pin" => state.config.update.pin || :null,
      "mode" => "notify",
      "busy" => state.job != nil,
      "running" => Engine.running_document()
    })
  end
end
