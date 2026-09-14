defmodule Arc.CLIToolsTest do
  use ExUnit.Case, async: false

  alias Arc.CLI.ToolRegistry
  alias Arc.Data.Agent
  alias Arc.Data.CapabilityDiscovery
  alias Arc.Identity
  alias Arc.Identity.KeyStore

  setup do
    Arc.Control.Local.reset()
    System.delete_env("ARC_KEY")

    tool_dir =
      Path.join(System.tmp_dir!(), "arc_cli_tools_#{System.unique_integer([:positive])}")

    trust_dir =
      Path.join(System.tmp_dir!(), "arc_cli_trust_#{System.unique_integer([:positive])}")

    old_tool_dir = Application.get_env(:arc_cli, :tool_registry_dir)
    old_trust_dir = Application.get_env(:arc_cli, :trust_store_dir)
    Application.put_env(:arc_cli, :tool_registry_dir, tool_dir)
    Application.put_env(:arc_cli, :trust_store_dir, trust_dir)

    on_exit(fn ->
      System.delete_env("ARC_KEY")

      if old_tool_dir do
        Application.put_env(:arc_cli, :tool_registry_dir, old_tool_dir)
      else
        Application.delete_env(:arc_cli, :tool_registry_dir)
      end

      if old_trust_dir do
        Application.put_env(:arc_cli, :trust_store_dir, old_trust_dir)
      else
        Application.delete_env(:arc_cli, :trust_store_dir)
      end

      File.rm_rf!(tool_dir)
      File.rm_rf!(trust_dir)
    end)

    :ok
  end

  test "arc install and tool ls create an owner-scoped installed command" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = hello_provider_paths()

    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(client_id))
    end)

    install_output =
      ExUnit.CaptureIO.capture_io("y\n", fn ->
        Arc.CLI.main(["install", Identity.name(server_id), "primary"])
      end)

    list_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["tool", "ls"])
      end)

    assert install_output =~
             "Installed hello from #{Identity.name(server_id)}/primary [service/hello]"

    assert install_output =~ "Run: arc hello <request...>"
    assert list_output =~ "Owner: #{Identity.name(client_id)}"
    assert list_output =~ "Installed: 1"
    assert list_output =~ "hello -> #{Identity.name(server_id)}/primary [service/hello]"
    assert list_output =~ "Usage: arc hello <request...>"
    assert list_output =~ "Invocation: RAW /"
  end

  test "arc install --trust records the signer and skips the prompt" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = hello_provider_paths()

    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(client_id))
    end)

    install_output =
      ExUnit.CaptureIO.capture_io("", fn ->
        Arc.CLI.main(["install", "--trust", Identity.name(server_id), "primary"])
      end)

    refute install_output =~ "Trust this signer?"
    assert install_output =~ "Installed hello from #{Identity.name(server_id)}/primary"

    assert {:ok, [%{"state" => "allowed"}]} = Arc.CLI.TrustStore.list(client_id)
  end

  test "trust prompt answers only deny on an explicit no" do
    assert Arc.CLI.Tools.trust_answer("y\n") == :allow
    assert Arc.CLI.Tools.trust_answer("YES\n") == :allow
    assert Arc.CLI.Tools.trust_answer("n\n") == :deny
    assert Arc.CLI.Tools.trust_answer("no\n") == :deny
    assert Arc.CLI.Tools.trust_answer("\n") == :cancel
    assert Arc.CLI.Tools.trust_answer("maybe\n") == :cancel
    assert Arc.CLI.Tools.trust_answer(:eof) == :cancel
    assert Arc.CLI.Tools.trust_answer({:error, :terminated}) == :cancel
  end

  test "arc tool call invokes an installed capability" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = hello_provider_paths()

    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(client_id))
    end)

    ExUnit.CaptureIO.capture_io("y\n", fn ->
      Arc.CLI.main(["install", Identity.name(server_id), "primary"])
    end)

    output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["tool", "call", "hello", "GET /"])
      end)

    assert output =~ "provider hello"
  end

  test "stdin input source sends the piped body as the request payload" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, _manifest_path} = hello_provider_paths()

    manifest_path =
      Path.join(
        System.tmp_dir!(),
        "arc_stdin_manifest_#{System.unique_integer([:positive])}.json"
      )

    write_stdin_manifest(manifest_path)
    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    {:ok, input_device} =
      StringIO.open("# Title\n\nA long body with \"quotes\" and {{braces}}.\n")

    old_input_device = Application.get_env(:arc_cli, :stream_input_device)
    Application.put_env(:arc_cli, :stream_input_device, input_device)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(client_id))
      File.rm(manifest_path)

      if old_input_device do
        Application.put_env(:arc_cli, :stream_input_device, old_input_device)
      else
        Application.delete_env(:arc_cli, :stream_input_device)
      end
    end)

    ExUnit.CaptureIO.capture_io("y\n", fn ->
      Arc.CLI.main(["install", Identity.name(server_id), "primary"])
    end)

    help_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["pages", "write", "--help"])
      end)

    output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["pages", "write", "hrs/nb/page"])
      end)

    assert help_output =~ "Usage: arc pages write <path> < body"
    assert output =~ "/pages/hrs/nb/page\n# Title\n\nA long body with \"quotes\" and {{braces}}."
  end

  test "seal_to seals the stdin body per target and the open filter decodes the reply" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, _manifest_path} = hello_provider_paths()

    manifest_path =
      Path.join(System.tmp_dir!(), "arc_dm_manifest_#{System.unique_integer([:positive])}.json")

    write_dm_manifest(manifest_path)
    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    {:ok, input_device} = StringIO.open("a private line\n")
    old_input_device = Application.get_env(:arc_cli, :stream_input_device)
    Application.put_env(:arc_cli, :stream_input_device, input_device)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(client_id))
      File.rm(manifest_path)

      if old_input_device do
        Application.put_env(:arc_cli, :stream_input_device, old_input_device)
      else
        Application.delete_env(:arc_cli, :stream_input_device)
      end
    end)

    ExUnit.CaptureIO.capture_io("y\n", fn ->
      Arc.CLI.main(["install", Identity.name(server_id), "primary"])
    end)

    output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["dm", "send", Identity.name(server_id)])
      end)

    # The echo provider returns the header and both tokens. The header
    # carries the server's hex key. The first token is sealed to the server
    # and cannot be opened here. The second is sealed to the caller and
    # opens to the stdin body.
    # petnames turns the server's hex key into its name.
    assert output =~ "/dm/#{Identity.name(server_id)}\n[sealed: cannot open]\na private line\n"
    refute output =~ "sealed-v1:"
    refute output =~ Identity.encode_public_key(server_id)
  end

  test "a stdin command takes its body from an argument or a file before stdin" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, _manifest_path} = hello_provider_paths()

    manifest_path =
      Path.join(System.tmp_dir!(), "arc_dm_manifest_#{System.unique_integer([:positive])}.json")

    body_path =
      Path.join(System.tmp_dir!(), "arc_dm_body_#{System.unique_integer([:positive])}.md")

    write_dm_manifest(manifest_path)
    File.write!(body_path, "from a file\n")
    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    # stdin holds a body that must not be used when an argument or file wins.
    {:ok, input_device} = StringIO.open("from stdin\n")
    old_input_device = Application.get_env(:arc_cli, :stream_input_device)
    Application.put_env(:arc_cli, :stream_input_device, input_device)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(client_id))
      File.rm(manifest_path)
      File.rm(body_path)

      if old_input_device do
        Application.put_env(:arc_cli, :stream_input_device, old_input_device)
      else
        Application.delete_env(:arc_cli, :stream_input_device)
      end
    end)

    ExUnit.CaptureIO.capture_io("y\n", fn ->
      Arc.CLI.main(["install", Identity.name(server_id), "primary"])
    end)

    from_arg =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["dm", "send", Identity.name(server_id), "two", "words"])
      end)

    from_file =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["dm", "send", Identity.name(server_id), "--file", body_path])
      end)

    {result, missing} =
      ExUnit.CaptureIO.with_io(:stderr, fn ->
        Arc.CLI.main(["dm", "send", Identity.name(server_id), "--file", "/nope/none.md"])
      end)

    assert from_arg =~ "[sealed: cannot open]\ntwo words\n"
    assert from_file =~ "[sealed: cannot open]\nfrom a file\n"
    assert result == {:exit, 1}
    assert missing =~ "cannot read /nope/none.md"
  end

  test "installed tools can be invoked as top-level arc subcommands" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = hello_provider_paths()

    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(client_id))
    end)

    ExUnit.CaptureIO.capture_io("y\n", fn ->
      Arc.CLI.main(["install", Identity.name(server_id), "primary"])
    end)

    output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["hello", "GET /"])
      end)

    assert output =~ "provider hello"
  end

  test "installed tools are scoped to the active identity" do
    first_client = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = hello_provider_paths()

    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    second_client = Identity.generate()
    :ok = KeyStore.save(second_client)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(first_client))
      KeyStore.remove(Identity.name(second_client))
    end)

    ExUnit.CaptureIO.capture_io("y\n", fn ->
      Arc.CLI.main(["install", Identity.name(server_id), "primary"])
    end)

    System.put_env("ARC_KEY", Identity.name(second_client))

    list_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["tool", "ls"])
      end)

    assert list_output =~ "Owner: #{Identity.name(second_client)}"
    assert list_output =~ "Installed: 0"
    refute list_output =~ "hello ->"
  end

  test "sqlite capabilities can declare their own install alias and argv shape" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = sqlite_provider_paths()

    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(client_id))
    end)

    install_output =
      ExUnit.CaptureIO.capture_io("y\n", fn ->
        Arc.CLI.main(["install", Identity.name(server_id), "primary"])
      end)

    help_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["sqlite", "--help"])
      end)

    query_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["sqlite", "SELECT", "1", "AS", "n"])
      end)

    list_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["tool", "ls"])
      end)

    assert install_output =~
             "Installed sqlite from #{Identity.name(server_id)}/primary [data/sql]"

    assert install_output =~ "Run: arc sqlite <sql...>"
    assert help_output =~ "Execute a SQL statement against the remote SQLite database"
    assert help_output =~ "Usage: arc sqlite <sql...>"
    assert help_output =~ "Arguments:"
    assert help_output =~ "<sql...>"
    assert query_output =~ "n"
    assert query_output =~ "1 row(s)"
    assert list_output =~ "sqlite -> #{Identity.name(server_id)}/primary [data/sql]"
    assert list_output =~ "Usage: arc sqlite <sql...>"
  end

  test "install requires a published installable interface" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, _manifest_path} = hello_provider_paths()

    manifest_path =
      Path.join(
        System.tmp_dir!(),
        "arc_cli_capability_only_#{System.unique_integer([:positive])}.json"
      )

    File.write!(manifest_path, """
    {
      "release": {
        "version": "1.0.0",
        "channel": "stable"
      },
      "capability": {
        "id": "primary",
        "kind": "service",
        "scheme": "hello",
        "title": "Hello Service",
        "summary": "Capability document without an installable CLI interface.",
        "invocation": {
          "method": "RAW",
          "path": "/"
        }
      }
    }
    """)

    {:ok, client} = Agent.start_link(client_id)
    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(client)
    :ok = Agent.publish(server)

    on_exit(fn ->
      if Process.alive?(client), do: GenServer.stop(client, :normal)
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(client_id))
      File.rm(manifest_path)
    end)

    assert {:ok, detail} =
             CapabilityDiscovery.fetch_detail(client, Identity.name(server_id), "primary")

    assert {:error, :not_installable} = ToolRegistry.install(client_id, detail)
  end

  test "exec-served capabilities can install a REST-shaped namespace from a manifest-defined command tree" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    runtime_path = Path.expand("../../../../test/fixtures/providers/users-provider.exs", __DIR__)

    manifest_path =
      Path.expand("../../../../test/fixtures/providers/users-provider.json", __DIR__)

    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(client_id))
    end)

    install_output =
      ExUnit.CaptureIO.capture_io("y\n", fn ->
        Arc.CLI.main(["install", Identity.name(server_id), "primary"])
      end)

    help_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["users", "--help"])
      end)

    get_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["users", "get"])
      end)

    add_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["users", "add", "--name", "Ada", "--email", "ada@example.com"])
      end)

    assert install_output =~
             "Installed users from #{Identity.name(server_id)}/primary [service/rest]"

    assert install_output =~ "Run: arc users <subcommand>"
    assert help_output =~ "Manage users through the provider-authored ARC capability"
    assert help_output =~ "Usage: arc users <subcommand>"
    assert help_output =~ "Commands:"
    assert help_output =~ "get  List users"
    assert help_output =~ "add  Create a user"
    assert get_output =~ "ada@example.com"
    assert add_output =~ "ada@example.com"
  end

  test "installed tools can run manifest-declared stream commands" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = sandbox_provider_paths()

    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(client_id))
    end)

    install_output =
      ExUnit.CaptureIO.capture_io("y\n", fn ->
        Arc.CLI.main(["install", Identity.name(server_id), "primary"])
      end)

    new_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["sandbox", "new", "alpine"])
      end)

    {:ok, input_device} = StringIO.open("echo hi\nexit\n")
    {:ok, output_device} = StringIO.open("")
    old_input_device = Application.get_env(:arc_cli, :stream_input_device)
    Application.put_env(:arc_cli, :stream_input_device, input_device)

    on_exit(fn ->
      if old_input_device do
        Application.put_env(:arc_cli, :stream_input_device, old_input_device)
      else
        Application.delete_env(:arc_cli, :stream_input_device)
      end
    end)

    parent = self()

    pid =
      spawn(fn ->
        receive do
          {:go, output_device} ->
            Process.group_leader(self(), output_device)
            Arc.CLI.main(["sandbox", "shell", "sb-alpine"])
            send(parent, :shell_done)
        end
      end)

    ref = Process.monitor(pid)
    send(pid, {:go, output_device})

    assert_receive :shell_done, 5_000
    assert_receive {:DOWN, ^ref, :process, ^pid, :normal}, 5_000

    {_input, shell_output} = StringIO.contents(output_device)

    assert install_output =~
             "Installed sandbox from #{Identity.name(server_id)}/primary [compute/sandbox]"

    assert new_output =~ "sb-alpine"
    assert shell_output =~ "opened shell for sb-alpine"
    assert shell_output =~ "echo:echo hi"
    assert shell_output =~ "session closed"
  end

  test "tool info and verify report signed package metadata" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = hello_provider_paths()

    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(client_id))
    end)

    ExUnit.CaptureIO.capture_io("y\n", fn ->
      Arc.CLI.main(["install", Identity.name(server_id), "primary"])
    end)

    info_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["tool", "info", "hello"])
      end)

    verify_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["tool", "verify", "hello"])
      end)

    assert info_output =~ "Command: hello"
    assert info_output =~ "Version: 1.0.0"
    assert info_output =~ "Channel: stable"
    assert info_output =~ "Trust at install: allowed"
    assert verify_output =~ "Verified hello"
    assert verify_output =~ "Hash:"
  end

  test "tool pin and unpin toggle update tracking state" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    {runtime_path, manifest_path} = hello_provider_paths()

    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(client_id))
    end)

    ExUnit.CaptureIO.capture_io("y\n", fn ->
      Arc.CLI.main(["install", Identity.name(server_id), "primary"])
    end)

    pin_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["tool", "pin", "hello"])
      end)

    assert pin_output =~ "Pinned hello"
    assert {:ok, pinned} = ToolRegistry.get(client_id, "hello")
    assert pinned["pinned"] == true

    unpin_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["tool", "unpin", "hello"])
      end)

    assert unpin_output =~ "Unpinned hello"
    assert {:ok, unpinned} = ToolRegistry.get(client_id, "hello")
    assert unpinned["pinned"] == false
  end

  test "tool diff and update follow the provider stable channel" do
    client_id = persist_cli_identity()
    server_id = Identity.generate()
    runtime_path = Path.expand("../../../../test/fixtures/providers/hello-provider.exs", __DIR__)

    manifest_path =
      Path.join(
        System.tmp_dir!(),
        "arc_cli_update_manifest_#{System.unique_integer([:positive])}.json"
      )

    write_hello_manifest(manifest_path, "1.0.0", "Hello Service")
    File.chmod!(runtime_path, 0o755)

    {:ok, server} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server)

    on_exit(fn ->
      if Process.alive?(server), do: GenServer.stop(server, :normal)
      KeyStore.remove(Identity.name(client_id))
      File.rm(manifest_path)
    end)

    ExUnit.CaptureIO.capture_io("y\n", fn ->
      Arc.CLI.main(["install", Identity.name(server_id), "primary"])
    end)

    GenServer.stop(server, :normal)
    write_hello_manifest(manifest_path, "1.1.0", "Hello Service v2")

    {:ok, server2} =
      Agent.start_link(
        server_id,
        serve: "exec://#{runtime_path}?manifest=#{URI.encode_www_form(manifest_path)}"
      )

    :ok = Agent.publish(server2)

    on_exit(fn ->
      if Process.alive?(server2), do: GenServer.stop(server2, :normal)
    end)

    diff_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["tool", "diff", "hello"])
      end)

    update_output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["tool", "update", "hello"])
      end)

    assert diff_output =~ "release version: 1.0.0 -> 1.1.0"
    assert diff_output =~ "title: Hello Service -> Hello Service v2"
    assert update_output =~ "Installed hello"
    assert update_output =~ "Version: 1.1.0 (stable)"

    assert {:ok, updated} = ToolRegistry.get(client_id, "hello")
    assert updated["release_version"] == "1.1.0"
  end

  defp persist_cli_identity do
    identity = Identity.generate()
    :ok = KeyStore.save(identity)
    System.put_env("ARC_KEY", Identity.name(identity))
    identity
  end

  defp hello_provider_paths do
    runtime = Path.expand("../../../../test/fixtures/providers/hello-provider.exs", __DIR__)
    manifest = Path.expand("../../../../test/fixtures/providers/hello-provider.json", __DIR__)
    {runtime, manifest}
  end

  defp sqlite_provider_paths do
    runtime = Path.expand("../../../../test/fixtures/providers/sqlite-provider.exs", __DIR__)
    manifest = Path.expand("../../../../test/fixtures/providers/sqlite-provider.json", __DIR__)
    {runtime, manifest}
  end

  defp sandbox_provider_paths do
    runtime = Path.expand("../../../../test/fixtures/providers/sandbox-provider.exs", __DIR__)
    manifest = Path.expand("../../../../test/fixtures/providers/sandbox-provider.json", __DIR__)
    {runtime, manifest}
  end

  defp write_stdin_manifest(path) do
    manifest = %{
      "published_at" => "2026-03-06T00:00:00Z",
      "release" => %{"version" => "1.0.0", "channel" => "stable"},
      "capability" => %{
        "id" => "primary",
        "kind" => "service",
        "scheme" => "pages",
        "title" => "Pages",
        "summary" => "Write pages through the hello echo runtime.",
        "invocation" => %{"method" => "RAW", "path" => "/"}
      },
      "interfaces" => %{
        "cli" => %{
          "version" => 1,
          "namespace" => "pages",
          "commands" => [
            %{
              "path" => ["write"],
              "summary" => "Write a page body from stdin",
              "args" => [
                %{
                  "name" => "path",
                  "kind" => "positional",
                  "type" => "string",
                  "required" => true
                }
              ],
              "input" => %{"source" => "stdin", "template" => "POST /echo /pages/{{path}}"}
            }
          ]
        }
      }
    }

    File.write!(path, manifest |> :json.encode() |> IO.iodata_to_binary())
  end

  defp write_dm_manifest(path) do
    manifest = %{
      "published_at" => "2026-03-06T00:00:00Z",
      "release" => %{"version" => "1.0.0", "channel" => "stable"},
      "capability" => %{
        "id" => "primary",
        "kind" => "service",
        "scheme" => "dm",
        "title" => "DM",
        "summary" => "Sealed messages through the hello echo runtime.",
        "invocation" => %{"method" => "RAW", "path" => "/"}
      },
      "interfaces" => %{
        "cli" => %{
          "version" => 1,
          "namespace" => "dm",
          "commands" => [
            %{
              "path" => ["send"],
              "summary" => "Send a sealed body from stdin",
              "args" => [
                %{"name" => "to", "kind" => "positional", "type" => "string", "required" => true},
                %{
                  "name" => "text",
                  "kind" => "positional",
                  "type" => "string",
                  "required" => false,
                  "variadic" => true
                },
                %{"name" => "file", "kind" => "option", "flag" => "--file", "type" => "string"}
              ],
              "input" => %{
                "source" => "stdin",
                "template" => "POST /echo /dm/{{to|pubkey}}",
                "seal_to" => ["to", "me"],
                "body" => "text",
                "file" => "file"
              },
              "output" => %{"filter" => ["open", "petnames"]}
            }
          ]
        }
      }
    }

    File.write!(path, manifest |> :json.encode() |> IO.iodata_to_binary())
  end

  defp write_hello_manifest(path, version, title) do
    manifest = %{
      "published_at" => "2026-03-06T00:00:00Z",
      "release" => %{"version" => version, "channel" => "stable"},
      "capability" => %{
        "id" => "primary",
        "kind" => "service",
        "scheme" => "hello",
        "title" => title,
        "summary" =>
          "A provider-authored hello capability served through the generic ARC exec runtime.",
        "invocation" => %{"method" => "RAW", "path" => "/"},
        "examples" => ["GET /", "POST /echo hello"]
      },
      "interfaces" => %{
        "cli" => %{
          "version" => 1,
          "namespace" => "hello",
          "summary" => "Send a raw ARC request to the remote hello service",
          "commands" => [
            %{
              "path" => [],
              "summary" => "Forward one request to the remote hello service",
              "args" => [
                %{
                  "name" => "request",
                  "kind" => "positional",
                  "type" => "string",
                  "required" => true,
                  "variadic" => true,
                  "description" => "Request line and optional body"
                }
              ],
              "input" => %{"source" => "template", "template" => "{{request}}"}
            }
          ]
        }
      }
    }

    File.write!(path, manifest |> :json.encode() |> IO.iodata_to_binary())
  end
end
