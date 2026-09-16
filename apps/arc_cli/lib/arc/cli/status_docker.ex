defmodule Arc.CLI.StatusDocker do
  @moduledoc """
  Bounded, read-only inspection of ARC's local Compose containers.

  The snapshot is intentionally limited to public `docker ps` columns. It does
  not inspect containers, logs, configuration, environment variables, or
  mounted data.
  """

  @timeout_ms 1_000
  @max_output_bytes 1_048_576

  @format """
          {"service":{{printf "%q" (.Label "com.docker.compose.service")}},"name":{{printf "%q" .Names}},"image":{{printf "%q" .Image}},"state":{{printf "%q" .State}},"health":{{printf "%q" .Status}},"ports":{{printf "%q" .Ports}}}
          """
          |> String.trim()

  @type snapshot :: %{
          required(String.t()) => String.t() | [map()]
        }

  @doc """
  Returns public metadata for the Compose project, without raising when Docker
  is unavailable.

  The optional keyword list is reserved for tests. Production callers use the
  default Docker executable and timeout.
  """
  @spec snapshot(String.t(), keyword()) :: snapshot()
  def snapshot(project \\ "arc-local", opts \\ [])

  def snapshot(project, _opts) when not is_binary(project) do
    unavailable("")
  end

  def snapshot(project, _opts) when byte_size(project) == 0 or byte_size(project) > 255 do
    unavailable(project)
  end

  def snapshot(project, opts) do
    executable = Keyword.get_lazy(opts, :executable, fn -> System.find_executable("docker") end)
    timeout_ms = Keyword.get(opts, :timeout_ms, @timeout_ms)

    case executable do
      nil ->
        %{"status" => "not_installed", "project" => project, "services" => []}

      executable when is_binary(executable) ->
        args =
          Keyword.get(opts, :prefix_args, []) ++
            [
              "ps",
              "--all",
              "--filter",
              "label=com.docker.compose.project=#{project}",
              "--format",
              @format
            ]

        case run(executable, args, timeout_ms) do
          {:ok, output} ->
            case decode_services(output) do
              {:ok, services} -> %{"status" => "ok", "project" => project, "services" => services}
              {:error, _reason} -> unavailable(project)
            end

          {:error, _reason} ->
            unavailable(project)
        end
    end
  end

  @doc false
  @spec decode_services(String.t()) :: {:ok, [map()]} | {:error, :invalid_output}
  def decode_services(output) when is_binary(output) do
    case output |> String.split("\n", trim: true) |> Enum.map(&decode_service/1) |> collect() do
      {:ok, services} -> {:ok, Enum.sort_by(services, &{&1["service"], &1["name"]})}
      _ -> {:error, :invalid_output}
    end
  end

  defp unavailable(project),
    do: %{"status" => "unavailable", "project" => project, "services" => []}

  defp run(executable, args, timeout_ms) when is_integer(timeout_ms) and timeout_ms > 0 do
    port =
      Port.open({:spawn_executable, String.to_charlist(executable)}, [
        :binary,
        :exit_status,
        :use_stdio,
        :stderr_to_stdout,
        args: Enum.map(args, &String.to_charlist/1)
      ])

    collect(port, "", System.monotonic_time(:millisecond) + timeout_ms)
  rescue
    ArgumentError -> {:error, :spawn_failed}
  end

  defp run(_executable, _args, _timeout_ms), do: {:error, :invalid_timeout}

  defp collect(port, output, deadline) do
    remaining = max(deadline - System.monotonic_time(:millisecond), 0)

    receive do
      {^port, {:data, data}} ->
        if byte_size(output) + byte_size(data) > @max_output_bytes do
          terminate(port)
          {:error, :output_too_large}
        else
          collect(port, output <> data, deadline)
        end

      {^port, {:exit_status, 0}} ->
        {:ok, output}

      {^port, {:exit_status, _status}} ->
        {:error, :command_failed}
    after
      remaining ->
        terminate(port)
        {:error, :timeout}
    end
  end

  defp terminate(port) do
    terminate_process(port)
    Port.close(port)
  rescue
    ArgumentError -> :ok
  end

  defp terminate_process(port) do
    with {:os_pid, pid} when is_integer(pid) <- Port.info(port, :os_pid),
         executable when is_binary(executable) <- System.find_executable("kill") do
      _ = System.cmd(executable, ["-TERM", Integer.to_string(pid)], stderr_to_stdout: true)
    else
      _ -> :ok
    end
  end

  defp decode_service(line) do
    with %{} = record <- decode_json(line),
         true <-
           Map.keys(record) |> Enum.sort() == [
             "health",
             "image",
             "name",
             "ports",
             "service",
             "state"
           ],
         true <- Enum.all?(record, fn {_key, value} -> is_binary(value) end) do
      {:ok,
       %{
         "service" => string(record["service"]),
         "name" => string(record["name"]),
         "image" => string(record["image"]),
         "state" => string(record["state"]),
         "health" => health(record["health"]),
         "ports" => string(record["ports"])
       }}
    else
      _ -> {:error, :invalid_output}
    end
  end

  defp collect(results) do
    Enum.reduce_while(results, {:ok, []}, fn
      {:ok, service}, {:ok, services} -> {:cont, {:ok, [service | services]}}
      {:error, reason}, _services -> {:halt, {:error, reason}}
    end)
    |> then(fn
      {:ok, services} -> {:ok, Enum.reverse(services)}
      error -> error
    end)
  end

  defp decode_json(line) do
    :json.decode(line)
  rescue
    _ -> nil
  end

  defp string(value) when is_binary(value), do: value
  defp string(_value), do: ""

  defp health(status) when is_binary(status) do
    case Regex.run(~r/\((healthy|unhealthy|health: starting)\)/, status) do
      [_, value] -> value
      _ -> nil
    end
  end

  defp health(_status), do: nil
end
