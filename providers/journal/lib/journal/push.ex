defmodule Journal.Push do
  @moduledoc "Debounced git push to JOURNAL_REMOTE after commits. No-op without a remote."
  use GenServer

  @debounce_ms 30_000

  def start_link(root), do: GenServer.start_link(__MODULE__, root, name: __MODULE__)

  def touch, do: GenServer.cast(__MODULE__, :touch)

  @impl true
  def init(root), do: {:ok, %{root: root, timer: nil, remote: Journal.Config.remote()}}

  @impl true
  def handle_cast(:touch, %{remote: nil} = state), do: {:noreply, state}

  def handle_cast(:touch, state) do
    if state.timer, do: Process.cancel_timer(state.timer)
    {:noreply, %{state | timer: Process.send_after(self(), :push, @debounce_ms)}}
  end

  @impl true
  def handle_info(:push, state) do
    case Journal.Git.push(Path.join(state.root, "repo"), state.remote) do
      {_, 0} -> :ok
      {out, _} -> IO.puts(:stderr, "journal: push failed: " <> String.trim(out))
    end

    {:noreply, %{state | timer: nil}}
  end
end
