defmodule Arc.Data.CapabilityInvocationTest do
  use ExUnit.Case, async: false

  alias Arc.Identity
  alias Arc.Data.Agent
  alias Arc.Data.CapabilityInvocation

  setup do
    Arc.Control.Local.reset()
    :ok
  end

  test "invoke uses mounted provider and invocation metadata" do
    client_id = Identity.generate()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = invoke_provider_paths()

    {:ok, client} = Agent.start_link(client_id)
    File.chmod!(runtime_path, 0o755)

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
        "invocation" => %{"method" => "RUN", "path" => "/task"}
      }
    }

    assert {:ok, reply} = CapabilityInvocation.invoke(client, mount, "do-work")

    decoded = :json.decode(reply.text)
    assert decoded["message"] == "do-work"
    assert decoded["method"] == "RUN"
    assert decoded["path"] == "/task"
  end

  test "invoke works from a new agent process using the same identity" do
    client_id = Identity.generate()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = invoke_provider_paths()

    {:ok, client_a} = Agent.start_link(client_id)
    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(client_a)
    :ok = Agent.publish(server)

    mount = %{
      "provider" => %{
        "name" => Identity.name(server_id),
        "short_name" => Identity.short_name(server_id)
      },
      "capability" => %{
        "id" => "primary",
        "invocation" => %{"method" => "RUN", "path" => "/task"}
      }
    }

    assert {:ok, _reply} = CapabilityInvocation.invoke(client_a, mount, "warmup")
    GenServer.stop(client_a, :normal)

    {:ok, client_b} = Agent.start_link(client_id)
    :ok = Agent.publish(client_b)

    assert {:ok, reply} = CapabilityInvocation.invoke(client_b, mount, "after-restart")

    decoded = :json.decode(reply.text)
    assert decoded["message"] == "after-restart"
    assert decoded["method"] == "RUN"
    assert decoded["path"] == "/task"
  end

  test "invoke reaches an HTTP-backed capability" do
    client_id = Identity.generate()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = hello_provider_paths()

    {:ok, client} = Agent.start_link(client_id)
    File.chmod!(runtime_path, 0o755)

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
        "invocation" => %{"method" => "RAW", "path" => "/"}
      }
    }

    assert {:ok, reply} = CapabilityInvocation.invoke(client, mount, "GET /")
    assert reply.text =~ "provider hello"
  end

  test "stream invocations support open, data, resize, and exit" do
    client_id = Identity.generate()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = sandbox_provider_paths()

    {:ok, client} = Agent.start_link(client_id)
    File.chmod!(runtime_path, 0o755)

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
          "path" => "/sandbox/shell",
          "stream" => %{"tty" => true}
        }
      }
    }

    assert {:ok, stream} =
             CapabilityInvocation.open_stream(client, mount, "SHELL sb-alpine")

    assert {:ok, open_msg} = CapabilityInvocation.recv_stream(client, stream, timeout_ms: 5_000)
    assert open_msg.kind == :stream_data
    assert open_msg.text =~ "opened shell for sb-alpine"

    assert :ok = CapabilityInvocation.resize_stream(client, stream, 80, 24)
    assert {:ok, resize_msg} = CapabilityInvocation.recv_stream(client, stream, timeout_ms: 5_000)
    assert resize_msg.kind == :stream_data
    assert resize_msg.text =~ "resize:80x24"

    assert :ok = CapabilityInvocation.send_stream_data(client, stream, "echo hi\n")
    assert {:ok, data_msg} = CapabilityInvocation.recv_stream(client, stream, timeout_ms: 5_000)
    assert data_msg.kind == :stream_data
    assert data_msg.text =~ "echo:echo hi"

    assert :ok = CapabilityInvocation.send_stream_data(client, stream, "exit\n")
    assert {:ok, exit_msg} = CapabilityInvocation.recv_stream(client, stream, timeout_ms: 5_000)
    assert exit_msg.kind == :stream_exit
    assert exit_msg.meta["status"] == 0
  end

  defp hello_provider_paths do
    runtime = Path.expand("../../../../../test/fixtures/providers/hello-provider.exs", __DIR__)
    manifest = Path.expand("../../../../../test/fixtures/providers/hello-provider.json", __DIR__)
    {runtime, manifest}
  end

  defp sandbox_provider_paths do
    runtime = Path.expand("../../../../../test/fixtures/providers/sandbox-provider.exs", __DIR__)

    manifest =
      Path.expand("../../../../../test/fixtures/providers/sandbox-provider.json", __DIR__)

    {runtime, manifest}
  end

  defp invoke_provider_paths do
    runtime_path =
      System.tmp_dir!()
      |> Path.join("arc_invoke_provider_#{System.unique_integer([:positive])}.exs")

    manifest_path =
      System.tmp_dir!()
      |> Path.join("arc_invoke_provider_#{System.unique_integer([:positive])}.json")

    File.write!(runtime_path, """
    #!/usr/bin/env elixir
    for line <- IO.stream(:stdio, :line) do
      payload = :json.decode(line)

      reply =
        %{
          "message" => payload["message"],
          "method" => get_in(payload, ["meta", "method"]),
          "path" => get_in(payload, ["meta", "path"])
        }

      IO.binwrite(IO.iodata_to_binary(:json.encode(%{"reply" => reply})) <> "\\n")
    end
    """)

    File.write!(manifest_path, """
    {
      "release": {
        "version": "1.0.0",
        "channel": "stable"
      },
      "capability": {
        "id": "primary",
        "kind": "compute",
        "scheme": "invoke",
        "title": "Invoke Echo",
        "summary": "Echo invocation context",
        "invocation": {
          "method": "RUN",
          "path": "/task"
        }
      }
    }
    """)

    on_exit(fn ->
      File.rm(runtime_path)
      File.rm(manifest_path)
    end)

    {runtime_path, manifest_path}
  end
end
