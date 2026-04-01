defmodule Arc.Host.ServiceTest do
  use ExUnit.Case, async: false

  setup do
    Arc.Control.Local.reset()
    :ok
  end

  alias Arc.Data.Agent
  alias Arc.Data.CapabilityPackage
  alias Arc.Data.Toolbox
  alias Arc.Host.Client
  alias Arc.Host.Service
  alias Arc.Identity
  alias Arc.Identity.KeyStore

  test "host serves status initialize info and call over the local socket" do
    socket_path = tmp_socket_path("arc_host_service")
    {:ok, client_id} = KeyStore.generate()
    {:ok, server_id} = KeyStore.generate()
    {runtime_path, manifest_path} = hello_provider_paths()
    File.chmod!(runtime_path, 0o755)

    on_exit(fn ->
      _ = File.rm(socket_path)
      _ = KeyStore.remove(Identity.name(client_id))
      _ = KeyStore.remove(Identity.name(server_id))
    end)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)
    Process.sleep(25)

    {:ok, host} = Service.start_link(socket_path: socket_path)
    host_ref = Process.monitor(host)
    {:ok, admin_token} = Service.read_admin_token(socket_path)
    {:ok, delegated} = issue_token(socket_path, admin_token, Identity.name(client_id))

    assert {:ok, status} = Client.request(socket_path, "status")
    assert status["socket_path"] == socket_path
    assert status["identity_count"] == 0
    assert status["protocol_version"] == 1

    assert {:ok, protocol} = Client.request(socket_path, "protocol.describe")
    assert protocol["version"] == 1
    assert protocol["transport"]["kind"] == "unix_socket_ndjson"

    op_names = Enum.map(protocol["operations"], & &1["name"])
    assert "initialize" in op_names
    assert "tool.invoke" in op_names
    assert "stream.open" in op_names

    {:ok, socket} = Client.connect(socket_path)

    assert {:ok, initialized} =
             Client.request_connected(socket, "initialize", %{
               "token" => delegated["token"]
             })

    assert get_in(initialized, ["identity", "name"]) == Identity.name(client_id)

    assert Enum.sort(get_in(initialized, ["token", "scopes"])) ==
             Enum.sort(Arc.Host.Token.delegated_scopes())

    assert {:ok, current} = Client.request_connected(socket, "identity.current")
    assert current["public_key"] == Base.encode16(client_id.public_key, case: :lower)

    assert {:ok, info} =
             Client.request_connected(socket, "info", %{
               "peer" => Identity.name(server_id)
             })

    assert get_in(info, ["provider", "name"]) == Identity.name(server_id)

    assert {:ok, reply} =
             Client.request_connected(socket, "call", %{
               "peer" => Identity.name(server_id),
               "capability" => "primary",
               "input" => "hello host"
             })

    assert reply["kind"] == "response"
    assert reply["text"] =~ "provider hello"

    :ok = Client.close(socket)

    assert {:ok, %{"stopping" => true}} =
             Client.request(socket_path, "shutdown", %{}, token: admin_token)

    assert_receive {:DOWN, ^host_ref, :process, _pid, _reason}, 2_000
  end

  test "host opens, resizes, writes to, and closes stream sessions over the local socket" do
    socket_path = tmp_socket_path("arc_host_streams")
    {:ok, client_id} = KeyStore.generate()
    {:ok, server_id} = KeyStore.generate()
    {runtime_path, manifest_path} = pty_provider_paths()
    File.chmod!(runtime_path, 0o755)

    on_exit(fn ->
      _ = File.rm(socket_path)
      _ = KeyStore.remove(Identity.name(client_id))
      _ = KeyStore.remove(Identity.name(server_id))
    end)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)
    Process.sleep(25)

    {:ok, host} = Service.start_link(socket_path: socket_path)
    {:ok, admin_token} = Service.read_admin_token(socket_path)
    {:ok, delegated} = issue_token(socket_path, admin_token, Identity.name(client_id))

    {:ok, socket} = Client.connect(socket_path)

    assert {:ok, _initialized} =
             Client.request_connected(socket, "initialize", %{
               "token" => delegated["token"]
             })

    assert {:ok, opened} =
             Client.request_connected(socket, "stream.open", %{
               "peer" => Identity.name(server_id),
               "capability" => "primary",
               "input" => "SHELL"
             })

    app_session_id = opened["app_session_id"]
    assert is_binary(app_session_id)

    assert {:ok, open_event} =
             recv_event_until(socket, &(&1["text"] =~ "opened pty shell"), 5_000)

    assert open_event["op"] == "event"
    assert open_event["event"] == "stream_data"
    assert open_event["app_session_id"] == app_session_id

    assert {:ok, _resized} =
             Client.request_connected(socket, "stream.resize", %{
               "app_session_id" => app_session_id,
               "cols" => 120,
               "rows" => 40
             })

    assert {:ok, resize_event} =
             recv_event_until(socket, &(&1["text"] =~ "resize:120x40"), 5_000)

    assert resize_event["event"] == "stream_data"
    assert resize_event["text"] =~ "resize:120x40"

    assert {:ok, _written} =
             Client.request_connected(socket, "stream.write", %{
               "app_session_id" => app_session_id,
               "data" => "expr 21 + 21\n"
             })

    assert {:ok, data_event} = recv_event_until(socket, &(&1["text"] =~ "42"), 5_000)
    assert data_event["event"] == "stream_data"
    assert data_event["text"] =~ "42"

    assert {:ok, _closing} =
             Client.request_connected(socket, "stream.close", %{
               "app_session_id" => app_session_id
             })

    assert {:ok, exit_event} = recv_event_until(socket, &(&1["event"] == "stream_exit"), 5_000)
    assert exit_event["event"] == "stream_exit"
    assert exit_event["app_session_id"] == app_session_id
    assert exit_event["status"] == 0

    :ok = Client.close(socket)
    Service.shutdown(host)
  end

  test "host lists and invokes installed tools for the bound identity" do
    socket_path = tmp_socket_path("arc_host_tools")
    tool_dir = tmp_dir("arc_host_tools_registry")
    previous_tool_dir = Application.get_env(:arc_cli, :tool_registry_dir)
    Application.put_env(:arc_cli, :tool_registry_dir, tool_dir)

    {:ok, client_id} = KeyStore.generate()
    {:ok, hello_id} = KeyStore.generate()
    {:ok, sandbox_id} = KeyStore.generate()
    {hello_runtime, hello_manifest} = hello_provider_paths()
    {sandbox_runtime, sandbox_manifest} = sandbox_provider_paths()
    File.chmod!(hello_runtime, 0o755)
    File.chmod!(sandbox_runtime, 0o755)

    on_exit(fn ->
      if previous_tool_dir do
        Application.put_env(:arc_cli, :tool_registry_dir, previous_tool_dir)
      else
        Application.delete_env(:arc_cli, :tool_registry_dir)
      end

      _ = File.rm(socket_path)
      _ = File.rm_rf(tool_dir)
      _ = KeyStore.remove(Identity.name(client_id))
      _ = KeyStore.remove(Identity.name(hello_id))
      _ = KeyStore.remove(Identity.name(sandbox_id))
    end)

    {:ok, hello_server} =
      Agent.start_link(
        hello_id,
        serve: "exec://#{hello_runtime}?manifest=#{URI.encode_www_form(hello_manifest)}"
      )

    {:ok, sandbox_server} =
      Agent.start_link(
        sandbox_id,
        serve: "exec://#{sandbox_runtime}?manifest=#{URI.encode_www_form(sandbox_manifest)}"
      )

    :ok = Agent.publish(hello_server)
    :ok = Agent.publish(sandbox_server)
    Process.sleep(25)

    write_tool_registry(client_id, tool_dir, [
      install_record("hello", hello_id, load_package!(hello_manifest)),
      install_record("sandbox", sandbox_id, load_package!(sandbox_manifest))
    ])

    {:ok, host} = Service.start_link(socket_path: socket_path)
    {:ok, admin_token} = Service.read_admin_token(socket_path)
    {:ok, delegated} = issue_token(socket_path, admin_token, Identity.name(client_id))
    {:ok, socket} = Client.connect(socket_path)

    assert {:ok, _initialized} =
             Client.request_connected(socket, "initialize", %{
               "token" => delegated["token"]
             })

    assert {:ok, listed} = Client.request_connected(socket, "tool.list")
    assert Enum.map(listed["tools"], & &1["command"]) == ["hello", "sandbox"]

    assert {:ok, reply} =
             Client.request_connected(socket, "tool.invoke", %{
               "command" => "hello",
               "argv" => ["hello", "from", "host"]
             })

    assert reply["kind"] == "response"
    assert reply["text"] =~ "provider hello"

    assert {:ok, opened} =
             Client.request_connected(socket, "tool.invoke", %{
               "command" => "sandbox",
               "argv" => ["shell", "sb-demo"]
             })

    assert opened["mode"] == "stream"
    app_session_id = opened["app_session_id"]
    assert is_binary(app_session_id)

    assert {:ok, open_event} =
             recv_event_until(socket, &(&1["text"] =~ "opened shell for sb-demo"), 5_000)

    assert open_event["app_session_id"] == app_session_id

    assert {:ok, _written} =
             Client.request_connected(socket, "stream.write", %{
               "app_session_id" => app_session_id,
               "data" => "exit\n"
             })

    assert {:ok, exit_event} = recv_event_until(socket, &(&1["event"] == "stream_exit"), 5_000)
    assert exit_event["app_session_id"] == app_session_id

    :ok = Client.close(socket)
    Service.shutdown(host)
  end

  test "host denies operations outside the delegated token scope" do
    socket_path = tmp_socket_path("arc_host_scope_denial")
    {:ok, client_id} = KeyStore.generate()
    {:ok, host} = Service.start_link(socket_path: socket_path)
    {:ok, admin_token} = Service.read_admin_token(socket_path)
    {:ok, delegated} = issue_token(socket_path, admin_token, Identity.name(client_id), ["info"])

    on_exit(fn ->
      _ = File.rm(socket_path)
      _ = KeyStore.remove(Identity.name(client_id))
    end)

    {:ok, socket} = Client.connect(socket_path)

    assert {:ok, _initialized} =
             Client.request_connected(socket, "initialize", %{
               "token" => delegated["token"]
             })

    assert {:error, {"forbidden", message}} =
             Client.request_connected(socket, "call", %{
               "peer" => "missing-peer",
               "capability" => "primary",
               "input" => "hello"
             })

    assert message =~ "call"

    :ok = Client.close(socket)
    Service.shutdown(host)
  end

  defp hello_provider_paths do
    runtime = Path.expand("../../../../../test/fixtures/providers/hello-provider.exs", __DIR__)
    manifest = Path.expand("../../../../../test/fixtures/providers/hello-provider.json", __DIR__)
    {runtime, manifest}
  end

  defp pty_provider_paths do
    runtime = Path.expand("../../../../../test/fixtures/providers/pty-shell-provider.py", __DIR__)

    manifest =
      Path.expand("../../../../../test/fixtures/providers/pty-shell-provider.json", __DIR__)

    {runtime, manifest}
  end

  defp sandbox_provider_paths do
    runtime = Path.expand("../../../../../test/fixtures/providers/sandbox-provider.exs", __DIR__)

    manifest =
      Path.expand("../../../../../test/fixtures/providers/sandbox-provider.json", __DIR__)

    {runtime, manifest}
  end

  defp recv_event_until(socket, matcher, timeout_ms, started_at \\ nil)

  defp recv_event_until(socket, matcher, timeout_ms, nil) do
    recv_event_until(socket, matcher, timeout_ms, System.monotonic_time(:millisecond))
  end

  defp recv_event_until(socket, matcher, timeout_ms, started_at) do
    elapsed = System.monotonic_time(:millisecond) - started_at

    if elapsed > timeout_ms do
      {:error, :timeout}
    else
      remaining = max(timeout_ms - elapsed, 100)

      case Client.recv(socket, remaining) do
        {:ok, %{"op" => "event"} = event} ->
          if matcher.(event) do
            {:ok, event}
          else
            recv_event_until(socket, matcher, timeout_ms, started_at)
          end

        {:ok, _other} ->
          recv_event_until(socket, matcher, timeout_ms, started_at)

        {:error, reason} ->
          {:error, reason}
      end
    end
  end

  defp tmp_socket_path(prefix) do
    Path.join(System.tmp_dir!(), "#{prefix}_#{System.unique_integer([:positive])}.sock")
  end

  defp issue_token(socket_path, admin_token, identity, scopes \\ nil) do
    params =
      %{"identity" => identity}
      |> maybe_put("scopes", scopes)

    Client.request(socket_path, "token.issue", params, token: admin_token)
  end

  defp maybe_put(map, _key, nil), do: map
  defp maybe_put(map, key, value), do: Map.put(map, key, value)

  defp tmp_dir(prefix) do
    path = Path.join(System.tmp_dir!(), "#{prefix}_#{System.unique_integer([:positive])}")
    File.mkdir_p!(path)
    path
  end

  defp load_package!(path) do
    {:ok, package} = CapabilityPackage.load_file(path)
    package
  end

  defp install_record(command, provider_id, package) do
    capability = package["capability"] || %{}

    %{
      "command" => command,
      "provider" => %{
        "name" => Identity.name(provider_id),
        "short_name" => Identity.short_name(provider_id),
        "public_key" => Identity.encode_public_key(provider_id)
      },
      "capability" => capability,
      "capability_id" => capability["id"],
      "usage" => Toolbox.usage_from_capability(command, capability),
      "release_version" => get_in(package, ["release", "version"]),
      "channel" => get_in(package, ["release", "channel"])
    }
  end

  defp write_tool_registry(owner, dir, tools) do
    owner_hex = Base.encode16(owner.public_key, case: :lower)
    owner_dir = Path.join(dir, owner_hex)
    File.mkdir_p!(owner_dir)

    body =
      %{
        "version" => 2,
        "owner_name" => Identity.name(owner),
        "owner_public_key" => Identity.encode_public_key(owner),
        "tools" => tools
      }
      |> :json.encode()
      |> IO.iodata_to_binary()

    File.write!(Path.join(owner_dir, "tools.json"), body)
  end
end
