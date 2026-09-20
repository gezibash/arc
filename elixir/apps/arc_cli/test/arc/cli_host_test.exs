defmodule Arc.CLIHostTest do
  use ExUnit.Case, async: false

  alias Arc.Host.Service
  alias Arc.Identity
  alias Arc.Identity.KeyStore

  test "arc host status and stop talk to the local host service" do
    socket_path = tmp_socket_path("arc_cli_host")
    {:ok, host} = Service.start_link(socket_path: socket_path)
    host_ref = Process.monitor(host)

    status_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["host", "status", "--socket", socket_path])
      end)

    assert status_output =~ "Socket: #{socket_path}"
    assert status_output =~ "Protocol: v1"
    assert status_output =~ "Relay: local only"

    stop_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["host", "stop", "--socket", socket_path])
      end)

    assert stop_output =~ "Stopping ARC host"
    assert_receive {:DOWN, ^host_ref, :process, _pid, _reason}, 2_000
  end

  test "arc host token issue mints delegated identity tokens" do
    socket_path = tmp_socket_path("arc_cli_host_token")
    {:ok, identity} = KeyStore.generate()
    {:ok, host} = Service.start_link(socket_path: socket_path)

    on_exit(fn ->
      Service.shutdown(host)
      _ = File.rm(socket_path)
      _ = KeyStore.remove(Identity.name(identity))
    end)

    output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main([
          "host",
          "token",
          "issue",
          "--socket",
          socket_path,
          "--identity",
          Identity.name(identity),
          "--scope",
          "call",
          "--scope",
          "stream"
        ])
      end)

    assert output =~ "Token: "
    assert output =~ "Identity: #{Identity.name(identity)}"
    assert output =~ "Scopes: call, stream"
  end

  defp tmp_socket_path(prefix) do
    Path.join(System.tmp_dir!(), "#{prefix}_#{System.unique_integer([:positive])}.sock")
  end
end
