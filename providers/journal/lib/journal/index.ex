defmodule Journal.Index do
  @moduledoc """
  Search index over the repo, backed by qmd. Reindexing runs in the
  background with a debounce so writes never wait on it.
  """
  use GenServer

  @debounce_ms 2_000

  def start_link(root), do: GenServer.start_link(__MODULE__, root, name: __MODULE__)

  def touch, do: GenServer.cast(__MODULE__, :touch)

  @doc "Run a search. Returns {:ok, text} or {:error, reason}."
  def search(root, query, deep?) do
    cmd = if deep?, do: "query", else: "search"

    case qmd(root, [cmd, query, "-n", "10", "--full-path"]) do
      {:ok, out} -> {:ok, out}
      error -> error
    end
  end

  def available?, do: System.find_executable("qmd") != nil

  @impl true
  def init(root) do
    setup(root)
    {:ok, %{root: root, timer: nil}}
  end

  @impl true
  def handle_cast(:touch, state) do
    if state.timer, do: Process.cancel_timer(state.timer)
    {:noreply, %{state | timer: Process.send_after(self(), :reindex, @debounce_ms)}}
  end

  @impl true
  def handle_info(:reindex, state) do
    qmd(state.root, ["update"])
    {:noreply, %{state | timer: nil}}
  end

  defp setup(root) do
    if available?() do
      unless File.dir?(Path.join(root, ".qmd")), do: qmd(root, ["init"])

      case qmd(root, ["collection", "list"]) do
        {:ok, out} ->
          unless String.contains?(out, "journal"),
            do:
              qmd(root, [
                "collection",
                "add",
                Path.join(root, "repo/projects"),
                "--name",
                "journal"
              ])

        _ ->
          :ok
      end
    end
  end

  defp qmd(root, args) do
    if available?() do
      case System.cmd("qmd", args, cd: root, env: [{"PWD", root}], stderr_to_stdout: true) do
        {out, 0} -> {:ok, out}
        {out, _} -> {:error, "qmd failed: " <> String.trim(out)}
      end
    else
      {:error, "search_unavailable"}
    end
  rescue
    e -> {:error, "qmd failed: " <> Exception.message(e)}
  end
end
