defmodule Arc.Data.PTYIntegrationTest do
  use ExUnit.Case, async: false

  alias Arc.Data.Agent
  alias Arc.Data.CapabilityInvocation
  alias Arc.Identity

  @moduletag :integration

  setup do
    Arc.Control.Local.reset()
    :ok
  end

  test "real PTY stream supports command output, resize, close, and exit" do
    client_id = Identity.generate()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = pty_provider_paths()

    File.chmod!(runtime_path, 0o755)

    {:ok, client} = Agent.start_link(client_id)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(client)
    :ok = Agent.publish(server)

    mount = %{
      "provider" => %{
        "name" => Identity.name(server_id),
        "short_name" => Identity.short_name(server_id)
      },
      "capability" => %{
        "id" => "primary",
        "invocation" => %{
          "mode" => "stream",
          "method" => "RAW",
          "path" => "/shell",
          "stream" => %{"tty" => true}
        }
      }
    }

    assert {:ok, stream} = CapabilityInvocation.open_stream(client, mount, "SHELL")

    assert {:ok, open_output} =
             recv_until_contains(client, stream, ["opened pty shell", "ARC> "], 15_000)

    assert open_output =~ "opened pty shell"
    assert open_output =~ "ARC> "

    assert :ok = CapabilityInvocation.send_stream_data(client, stream, "expr 21 + 21\n")

    assert {:ok, expr_output} =
             recv_until_contains(client, stream, ["42", "ARC> "], 15_000)

    assert expr_output =~ "42"

    assert :ok = CapabilityInvocation.resize_stream(client, stream, 100, 40)

    assert {:ok, resize_output} =
             recv_until_contains(client, stream, ["resize:100x40"], 15_000)

    assert resize_output =~ "resize:100x40"

    assert :ok = CapabilityInvocation.send_stream_data(client, stream, "stty size\n")

    assert {:ok, size_output} =
             recv_until_contains(client, stream, ["40 100", "ARC> "], 15_000)

    assert size_output =~ "40 100"

    assert :ok = CapabilityInvocation.close_stream(client, stream)

    assert {:ok, exit_output, exit_msg} = recv_until_exit(client, stream, 15_000)
    assert exit_output =~ "exit"
    assert exit_msg.meta["status"] == 0
  end

  defp recv_until_contains(client, stream, needles, timeout_ms, acc \\ "")

  defp recv_until_contains(_client, _stream, _needles, timeout_ms, _acc) when timeout_ms <= 0 do
    {:error, :timeout}
  end

  defp recv_until_contains(client, stream, needles, timeout_ms, acc) do
    started_at = System.monotonic_time(:millisecond)

    case CapabilityInvocation.recv_stream(client, stream, timeout_ms: min(timeout_ms, 2_000)) do
      {:ok, %{kind: :stream_data, text: text}} ->
        combined = normalize(acc <> text)

        if Enum.all?(needles, &String.contains?(combined, &1)) do
          {:ok, combined}
        else
          recv_until_contains(
            client,
            stream,
            needles,
            remaining(timeout_ms, started_at),
            combined
          )
        end

      {:ok, %{kind: :stream_exit} = msg} ->
        {:error, {:unexpected_exit, normalize(acc <> msg.text), msg}}

      {:ok, %{kind: :stream_error} = msg} ->
        {:error, {:stream_error, normalize(acc <> msg.text), msg}}

      {:error, :timeout} ->
        {:error, :timeout}

      other ->
        {:error, other}
    end
  end

  defp recv_until_exit(client, stream, timeout_ms, acc \\ "")

  defp recv_until_exit(_client, _stream, timeout_ms, _acc) when timeout_ms <= 0 do
    {:error, :timeout}
  end

  defp recv_until_exit(client, stream, timeout_ms, acc) do
    started_at = System.monotonic_time(:millisecond)

    case CapabilityInvocation.recv_stream(client, stream, timeout_ms: min(timeout_ms, 2_000)) do
      {:ok, %{kind: :stream_data, text: text}} ->
        recv_until_exit(client, stream, remaining(timeout_ms, started_at), normalize(acc <> text))

      {:ok, %{kind: :stream_exit} = msg} ->
        {:ok, normalize(acc <> msg.text), msg}

      {:ok, %{kind: :stream_error} = msg} ->
        {:error, {:stream_error, normalize(acc <> msg.text), msg}}

      {:error, :timeout} ->
        {:error, :timeout}

      other ->
        {:error, other}
    end
  end

  defp normalize(text) do
    text
    |> String.replace("\r", "")
  end

  defp remaining(timeout_ms, started_at) do
    timeout_ms - (System.monotonic_time(:millisecond) - started_at)
  end

  defp pty_provider_paths do
    runtime =
      Path.expand("../../../../../test/fixtures/providers/pty-shell-provider.py", __DIR__)

    manifest =
      Path.expand("../../../../../test/fixtures/providers/pty-shell-provider.json", __DIR__)

    {runtime, manifest}
  end
end
