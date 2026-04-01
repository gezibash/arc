defmodule Mix.Tasks.Arc.Relay.Load do
  use Mix.Task

  alias Arc.Net.Load

  @shortdoc "Run relay load harness with telemetry summary"
  @moduledoc """
  Run a repeatable relay load pass.

  Examples:

      mix arc.relay.load --host localhost --port 7331 --connections 5000
      mix arc.relay.load --host localhost --port 7331 --connections 2000 --packets 20000 --mode drop
      mix arc.relay.load --host localhost --port 7331 --connections 1000 --packets 5000 --mode forward
  """

  @telemetry_events [
    [:arc, :net, :relay, :packet, :forwarded],
    [:arc, :net, :relay, :packet, :dropped],
    [:arc, :net, :relay, :route, :registered],
    [:arc, :net, :relay, :route, :unregistered],
    [:arc, :net, :relay, :acceptor, :restarted],
    [:arc, :net, :relay, :shard, :restarted],
    [:arc, :net, :transport, :deliver, :fallback],
    [:arc, :net, :connection, :backpressure, :drop],
    [:arc, :net, :connection, :backpressure, :disconnect],
    [:arc, :net, :connection, :closed]
  ]

  @impl Mix.Task
  def run(args) do
    Mix.Task.run("app.start")

    {opts, _argv, _invalid} =
      OptionParser.parse(args,
        strict: [
          host: :string,
          port: :integer,
          connections: :integer,
          packets: :integer,
          payload_bytes: :integer,
          timeout_ms: :integer,
          mode: :string
        ],
        aliases: [h: :host, p: :port, c: :connections, n: :packets]
      )

    load_opts = [
      host: Keyword.get(opts, :host, "localhost"),
      port: Keyword.get(opts, :port, 7331),
      connections: Keyword.get(opts, :connections, 1000),
      packets: Keyword.get(opts, :packets, 0),
      payload_bytes: Keyword.get(opts, :payload_bytes, 64),
      timeout_ms: Keyword.get(opts, :timeout_ms, 2000),
      mode: normalize_mode(Keyword.get(opts, :mode, "drop"))
    ]

    {:ok, telemetry_agent} = Agent.start_link(fn -> initial_telemetry() end)
    handler_id = "arc-relay-load-#{System.unique_integer([:positive])}"

    :ok =
      :telemetry.attach_many(
        handler_id,
        @telemetry_events,
        &__MODULE__.handle_event/4,
        telemetry_agent
      )

    result = Load.run(load_opts)

    :telemetry.detach(handler_id)
    telemetry_summary = Agent.get(telemetry_agent, & &1)
    Agent.stop(telemetry_agent)

    print_result(result, telemetry_summary)
  end

  def handle_event([:arc, :net, :relay, :packet, :forwarded], measurements, _metadata, agent) do
    increment(agent, :relay_packet_forwarded, measurements[:count] || 1)
  end

  def handle_event([:arc, :net, :relay, :packet, :dropped], measurements, metadata, agent) do
    increment(agent, :relay_packet_dropped, measurements[:count] || 1)

    case metadata[:reason] do
      nil -> :ok
      reason -> increment(agent, {:relay_dropped_reason, reason}, measurements[:count] || 1)
    end
  end

  def handle_event([:arc, :net, :relay, :route, :registered], measurements, _metadata, agent) do
    increment(agent, :relay_route_registered, measurements[:count] || 1)
  end

  def handle_event([:arc, :net, :relay, :route, :unregistered], measurements, _metadata, agent) do
    increment(agent, :relay_route_unregistered, measurements[:count] || 1)
  end

  def handle_event([:arc, :net, :relay, :acceptor, :restarted], measurements, _metadata, agent) do
    increment(agent, :relay_acceptor_restarted, measurements[:count] || 1)
  end

  def handle_event([:arc, :net, :relay, :shard, :restarted], measurements, _metadata, agent) do
    increment(agent, :relay_shard_restarted, measurements[:count] || 1)
  end

  def handle_event([:arc, :net, :transport, :deliver, :fallback], measurements, metadata, agent) do
    increment(agent, :transport_fallback, measurements[:count] || 1)

    case metadata[:reason] do
      nil -> :ok
      reason -> increment(agent, {:transport_fallback_reason, reason}, measurements[:count] || 1)
    end
  end

  def handle_event([:arc, :net, :connection, :closed], measurements, metadata, agent) do
    increment(agent, :connection_closed, measurements[:count] || 1)

    case metadata[:reason] do
      nil -> :ok
      reason -> increment(agent, {:connection_closed_reason, reason}, measurements[:count] || 1)
    end
  end

  def handle_event(
        [:arc, :net, :connection, :backpressure, :drop],
        measurements,
        _metadata,
        agent
      ) do
    increment(agent, :connection_backpressure_drop, measurements[:count] || 1)
  end

  def handle_event(
        [:arc, :net, :connection, :backpressure, :disconnect],
        measurements,
        _metadata,
        agent
      ) do
    increment(agent, :connection_backpressure_disconnect, measurements[:count] || 1)
  end

  def handle_event(_event, _measurements, _metadata, _agent), do: :ok

  defp print_result({:ok, summary}, telemetry_summary) do
    Mix.shell().info("relay load summary")
    Mix.shell().info(inspect(summary, pretty: true, limit: :infinity))
    Mix.shell().info("telemetry summary")
    Mix.shell().info(inspect(telemetry_summary, pretty: true, limit: :infinity))
  end

  defp print_result({:error, reason}, telemetry_summary) do
    Mix.shell().error("relay load failed: #{inspect(reason)}")
    Mix.shell().info("telemetry summary")
    Mix.shell().info(inspect(telemetry_summary, pretty: true, limit: :infinity))
  end

  defp increment(agent, key, by) do
    Agent.update(agent, fn state -> Map.update(state, key, by, &(&1 + by)) end)
  end

  defp normalize_mode("forward"), do: :forward
  defp normalize_mode(:forward), do: :forward
  defp normalize_mode(_), do: :drop

  defp initial_telemetry do
    %{
      relay_packet_forwarded: 0,
      relay_packet_dropped: 0,
      relay_route_registered: 0,
      relay_route_unregistered: 0,
      relay_acceptor_restarted: 0,
      relay_shard_restarted: 0,
      transport_fallback: 0,
      connection_backpressure_drop: 0,
      connection_backpressure_disconnect: 0,
      connection_closed: 0
    }
  end
end
