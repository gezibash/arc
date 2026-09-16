defmodule Arc.CLI.StatusDockerTest do
  use ExUnit.Case, async: true

  alias Arc.CLI.StatusDocker

  test "decodes only public docker ps fields" do
    output =
      ~s|{"service":"relay","name":"arc-local-relay-1","image":"arc-local-services:0.3.1","state":"running","health":"Up 3 minutes (healthy)","ports":"127.0.0.1:17331->7331/tcp"}\n| <>
        ~s|{"service":"agora","name":"arc-local-agora-1","image":"arc-local-services:0.3.1","state":"exited","health":"Exited (0) 2 minutes ago","ports":""}\n|

    assert StatusDocker.decode_services(output) ==
             {:ok,
              [
                %{
                  "service" => "agora",
                  "name" => "arc-local-agora-1",
                  "image" => "arc-local-services:0.3.1",
                  "state" => "exited",
                  "health" => nil,
                  "ports" => ""
                },
                %{
                  "service" => "relay",
                  "name" => "arc-local-relay-1",
                  "image" => "arc-local-services:0.3.1",
                  "state" => "running",
                  "health" => "healthy",
                  "ports" => "127.0.0.1:17331->7331/tcp"
                }
              ]}
  end

  test "returns not installed when docker cannot be found" do
    assert StatusDocker.snapshot("arc-local", executable: nil) == %{
             "status" => "not_installed",
             "project" => "arc-local",
             "services" => []
           }
  end

  test "terminates its owned timed out docker process" do
    {executable, prefix_args, pid_path} = write_slow_docker()
    started_at = System.monotonic_time(:millisecond)

    assert StatusDocker.snapshot("--not-a-flag",
             executable: executable,
             prefix_args: prefix_args,
             timeout_ms: 250
           ) == %{
             "status" => "unavailable",
             "project" => "--not-a-flag",
             "services" => []
           }

    assert System.monotonic_time(:millisecond) - started_at < 1_000
    assert await_file(pid_path)
    assert {pid, ""} = pid_path |> File.read!() |> String.trim() |> Integer.parse()
    assert await_process_exit(pid)
  end

  test "keeps a custom project value inside the filter argument" do
    capture_path = tmp_path("args")
    {executable, prefix_args} = write_reporting_docker(capture_path)
    project = "--not-a-flag"

    assert %{"status" => "ok", "services" => []} =
             StatusDocker.snapshot(project, executable: executable, prefix_args: prefix_args)

    assert File.read!(capture_path)
           |> String.split("\n", trim: true) == [
             "ps",
             "--all",
             "--filter",
             "label=com.docker.compose.project=--not-a-flag",
             "--format",
             "{\"service\":{{printf \"%q\" (.Label \"com.docker.compose.service\")}},\"name\":{{printf \"%q\" .Names}},\"image\":{{printf \"%q\" .Image}},\"state\":{{printf \"%q\" .State}},\"health\":{{printf \"%q\" .Status}},\"ports\":{{printf \"%q\" .Ports}}}"
           ]
  end

  test "marks malformed docker output unavailable" do
    {executable, prefix_args} = write_output_docker("not json")

    assert StatusDocker.snapshot("arc-local", executable: executable, prefix_args: prefix_args) ==
             %{
               "status" => "unavailable",
               "project" => "arc-local",
               "services" => []
             }
  end

  defp write_slow_docker do
    path = tmp_path("slow")
    pid_path = tmp_path("pid")
    File.write!(path, "#!/bin/sh\necho $$ > #{shell_quote(pid_path)}\nexec sleep 5\n")
    File.chmod!(path, 0o700)

    on_exit(fn ->
      File.rm(path)
      File.rm(pid_path)
    end)

    {shell(), [path], pid_path}
  end

  defp write_reporting_docker(capture_path) do
    path = tmp_path("reporting")
    File.write!(path, "#!/bin/sh\nprintf '%s\\n' \"$@\" > #{shell_quote(capture_path)}\n")
    File.chmod!(path, 0o700)

    on_exit(fn ->
      File.rm(path)
      File.rm(capture_path)
    end)

    {shell(), [path]}
  end

  defp write_output_docker(output) do
    path = tmp_path("output")
    File.write!(path, "#!/bin/sh\nprintf '%s\\n' #{shell_quote(output)}\n")
    File.chmod!(path, 0o700)
    on_exit(fn -> File.rm(path) end)
    {shell(), [path]}
  end

  defp await_file(path), do: await(fn -> File.exists?(path) end)

  defp await_process_exit(pid) do
    kill = System.find_executable("kill") || raise "kill executable is unavailable"

    await(fn ->
      System.cmd(kill, ["-0", Integer.to_string(pid)], stderr_to_stdout: true) != {"", 0}
    end)
  end

  defp await(check, attempts \\ 50)
  defp await(check, 0), do: check.()

  defp await(check, attempts) do
    if check.() do
      true
    else
      Process.sleep(10)
      await(check, attempts - 1)
    end
  end

  defp tmp_path(suffix),
    do:
      Path.join(
        System.tmp_dir!(),
        "arc_status_docker_#{System.unique_integer([:positive])}_#{suffix}"
      )

  defp shell_quote(value), do: "'#{String.replace(value, "'", "'\\\"'\\\"'")}'"
  defp shell, do: System.find_executable("sh") || raise("sh executable is unavailable")
end
