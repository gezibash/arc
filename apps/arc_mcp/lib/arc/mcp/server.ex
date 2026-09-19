defmodule Arc.MCP.Server do
  @moduledoc """
  Task-scoped MCP session server for mounted ARC capabilities.
  """

  # HTTPServer stops sessions with GenServer.stop/2. A restart would leave an untracked session.
  use GenServer, restart: :temporary

  alias Arc.MCP
  alias Arc.MCP.ToolProjection

  @default_poll_ms 500

  @spec start_link(keyword()) :: GenServer.on_start()
  def start_link(opts) do
    GenServer.start_link(__MODULE__, opts)
  end

  @spec request(pid(), map()) :: map() | nil
  def request(server, request) when is_map(request) do
    GenServer.call(server, {:request, request}, 30_000)
  catch
    # The caller is the HTTP server. A failed session must not stop the other sessions.
    :exit, _reason ->
      error_response(request, -32_603, "session unavailable")
  end

  @spec subscribe(pid(), pid()) :: :ok
  def subscribe(server, subscriber) when is_pid(subscriber) do
    GenServer.call(server, {:subscribe, subscriber})
  end

  @spec unsubscribe(pid(), pid()) :: :ok
  def unsubscribe(server, subscriber) when is_pid(subscriber) do
    GenServer.cast(server, {:unsubscribe, subscriber})
  end

  @impl GenServer
  def init(opts) do
    task = Keyword.fetch!(opts, :task)
    agent = Keyword.fetch!(opts, :agent)
    owner = Keyword.fetch!(opts, :owner)
    poll_ms = Keyword.get(opts, :poll_ms, @default_poll_ms)
    registry_opts = Keyword.get(opts, :registry_opts, [])
    subscribers = MapSet.new(List.wrap(Keyword.get(opts, :notify)) |> Enum.filter(&is_pid/1))

    {:ok, fingerprint} = ToolProjection.fingerprint(owner, task, registry_opts: registry_opts)
    schedule_poll(poll_ms)

    {:ok,
     %{
       task: task,
       agent: agent,
       owner: owner,
       initialized?: false,
       poll_ms: poll_ms,
       protocol_version: MCP.protocol_version(),
       registry_opts: registry_opts,
       subscribers: subscribers,
       tools_fingerprint: fingerprint
     }}
  end

  @impl GenServer
  def handle_call({:request, request}, _from, state) do
    {response, state} = handle_message(request, state)
    {:reply, response, state}
  end

  def handle_call({:subscribe, subscriber}, _from, state) do
    Process.monitor(subscriber)
    {:reply, :ok, %{state | subscribers: MapSet.put(state.subscribers, subscriber)}}
  end

  @impl GenServer
  def handle_cast({:unsubscribe, subscriber}, state) do
    {:noreply, %{state | subscribers: MapSet.delete(state.subscribers, subscriber)}}
  end

  @impl GenServer
  def handle_info(:poll_tools, state) do
    schedule_poll(state.poll_ms)

    state =
      case ToolProjection.fingerprint(state.owner, state.task, registry_opts: state.registry_opts) do
        {:ok, fingerprint} when fingerprint != state.tools_fingerprint and state.initialized? ->
          state
          |> notify(%{
            "jsonrpc" => "2.0",
            "method" => "notifications/tools/list_changed",
            "params" => %{}
          })
          |> Map.put(:tools_fingerprint, fingerprint)

        {:ok, fingerprint} ->
          %{state | tools_fingerprint: fingerprint}

        {:error, _reason} ->
          state
      end

    {:noreply, state}
  end

  def handle_info({:DOWN, _ref, :process, subscriber, _reason}, state) do
    {:noreply, %{state | subscribers: MapSet.delete(state.subscribers, subscriber)}}
  end

  defp handle_message(%{"jsonrpc" => "2.0", "method" => method} = request, state)
       when is_binary(method) do
    case {Map.has_key?(request, "id"), method} do
      {true, "initialize"} ->
        params = if is_map(request["params"]), do: request["params"], else: %{}
        protocol_version = negotiate_protocol(Map.get(params, "protocolVersion"))

        state = %{state | protocol_version: protocol_version}
        {ok_response(request, initialize_result(state)), state}

      {true, "ping"} ->
        {ok_response(request, %{}), state}

      {true, "tools/list"} ->
        case ToolProjection.list(state.owner, state.task, registry_opts: state.registry_opts) do
          {:ok, descriptors} ->
            {ok_response(request, %{"tools" => Enum.map(descriptors, & &1.tool)}), state}

          {:error, reason} ->
            {error_response(request, -32_000, "tool listing failed: #{inspect(reason)}"), state}
        end

      {true, "tools/call"} ->
        {handle_tool_call(request, state), state}

      {true, _unknown} ->
        {error_response(request, -32_601, "method not found"), state}

      {false, "notifications/initialized"} ->
        {nil, %{state | initialized?: true}}

      {false, _notification} ->
        {nil, state}
    end
  end

  defp handle_message(request, state) when is_map(request) do
    {error_response(request, -32_600, "invalid request"), state}
  end

  defp handle_tool_call(%{"params" => params} = request, state) when is_map(params) do
    name = params["name"]
    arguments = params["arguments"] || %{}

    cond do
      not is_binary(name) or name == "" ->
        error_response(request, -32_602, "tools/call requires params.name")

      not is_map(arguments) ->
        error_response(request, -32_602, "tools/call requires params.arguments to be an object")

      true ->
        result =
          case ToolProjection.call(
                 state.agent,
                 state.owner,
                 state.task,
                 name,
                 arguments,
                 registry_opts: state.registry_opts
               ) do
            {:ok, tool_result} ->
              tool_result

            {:error, {:tool_not_found, tool_name}} ->
              ToolProjection.error_result("unknown mounted tool: #{tool_name}")

            {:error, {:invalid_arguments, message}} ->
              ToolProjection.error_result(message)

            {:error, {:tool_error, message}} ->
              ToolProjection.error_result(message)
          end

        ok_response(request, result)
    end
  end

  defp handle_tool_call(request, _state) do
    error_response(request, -32_602, "tools/call requires params")
  end

  defp initialize_result(state) do
    %{
      "protocolVersion" => state.protocol_version,
      "capabilities" => %{"tools" => %{"listChanged" => true}},
      "serverInfo" => %{
        "name" => MCP.server_name(),
        "version" => MCP.server_version()
      },
      "instructions" =>
        "Expose the authenticated ARC toolbox for task '#{state.task}'. Use tools/list to inspect the current working set, including the default send tool."
    }
  end

  defp negotiate_protocol(version) when is_binary(version) do
    if version in MCP.supported_protocol_versions(), do: version, else: MCP.protocol_version()
  end

  defp negotiate_protocol(_), do: MCP.protocol_version()

  defp ok_response(%{"id" => id}, result) do
    %{"jsonrpc" => "2.0", "id" => id, "result" => result}
  end

  defp error_response(request, code, message) do
    %{
      "jsonrpc" => "2.0",
      "id" => Map.get(request, "id"),
      "error" => %{"code" => code, "message" => message}
    }
  end

  defp notify(state, notification) do
    Enum.each(state.subscribers, fn subscriber ->
      send(subscriber, {:arc_mcp, notification})
    end)

    state
  end

  defp schedule_poll(poll_ms) do
    Process.send_after(self(), :poll_tools, poll_ms)
  end
end
