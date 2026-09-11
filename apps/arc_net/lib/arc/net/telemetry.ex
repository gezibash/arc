defmodule Arc.Net.Telemetry do
  @moduledoc """
  Small wrapper for Arc net telemetry events.

  Event name prefix: `[:arc, :net]`.
  """

  @prefix [:arc, :net]

  @spec execute([atom()], map(), map()) :: :ok
  def execute(event_suffix, measurements, metadata \\ %{})
      when is_list(event_suffix) and is_map(measurements) and is_map(metadata) do
    :telemetry.execute(@prefix ++ event_suffix, measurements, metadata)
  end
end
