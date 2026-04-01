defmodule Arc.CLIAppsTest do
  use ExUnit.Case, async: false

  alias Arc.CLI.ProviderBundle
  alias Arc.Data.Handler.Exec

  test "arc apps init scaffolds a provider bundle" do
    root = tmp_dir("arc_apps_init")

    output =
      ExUnit.CaptureIO.capture_io(fn ->
        Arc.CLI.main(["apps", "init", root])
      end)

    assert output =~ "Initialized ARC app bundle"
    assert output =~ "Arcfile"
    assert output =~ "manifest.json"
    assert output =~ "run.sh"
    assert output =~ "bin/arc serve #{Path.expand(root)}"

    assert File.exists?(Path.join(root, "Arcfile"))
    assert File.exists?(Path.join(root, "manifest.json"))
    assert File.exists?(Path.join(root, "run.sh"))

    arcfile = File.read!(Path.join(root, "Arcfile"))
    assert arcfile =~ ~s(type = "exec")
    assert arcfile =~ ~s(command = "./run.sh")
    assert arcfile =~ ~s(path = "./manifest.json")

    runtime = File.read!(Path.join(root, "run.sh"))
    assert runtime =~ "namespace="
    assert runtime =~ "hello from"

    manifest =
      root
      |> Path.join("manifest.json")
      |> File.read!()
      |> :json.decode()

    expected_namespace =
      root
      |> Path.basename()
      |> String.downcase()
      |> String.replace(~r/[^a-z0-9-]+/u, "-")
      |> String.replace(~r/-+/u, "-")
      |> String.trim("-")

    assert get_in(manifest, ["interfaces", "cli", "namespace"]) == expected_namespace
    assert get_in(manifest, ["capability", "scheme"]) == expected_namespace

    assert {:ok, serve_uri, _bundle} = ProviderBundle.resolve_serve_target(root)
    {:ok, state} = Exec.init(serve_uri)

    {:noreply, state} =
      Exec.handle_message("hi there", <<1::256>>, %{request_id: <<1::128>>, framed?: true}, state)

    line =
      receive do
        {port, {:data, {:eol, line}}} when port == state.port -> line
      after
        2_000 -> flunk("expected scaffold runtime reply")
      end

    assert {:emit, [event], _state} = Exec.handle_info({state.port, {:data, {:eol, line}}}, state)
    assert event.body =~ "hello from #{expected_namespace}: hi there"
  end

  test "provider bundle resolves directory targets into exec URIs" do
    root = tmp_dir("arc_bundle_dir")
    {:ok, _files} = ProviderBundle.init(root)

    assert {:ok, serve_uri, bundle} = ProviderBundle.resolve_serve_target(root)
    assert serve_uri =~ "exec://"
    assert serve_uri =~ "manifest="
    assert bundle.root == Path.expand(root)
    assert bundle.runtime.command == Path.join(Path.expand(root), "run.sh")
    assert bundle.manifest.path == Path.join(Path.expand(root), "manifest.json")
  end

  test "bundle runtime args are passed through to exec" do
    root = tmp_dir("arc_bundle_args")
    runtime = Path.expand("../../../../test/fixtures/providers/hello-provider.exs", __DIR__)
    manifest = Path.expand("../../../../test/fixtures/providers/hello-provider.json", __DIR__)
    elixir = System.find_executable("elixir")

    File.mkdir_p!(root)

    File.write!(
      Path.join(root, "Arcfile"),
      """
      version = 1

      [runtime]
      type = "exec"
      command = "#{elixir}"
      args = ["#{runtime}"]
      cwd = "."

      [manifest]
      path = "#{manifest}"
      """
    )

    assert {:ok, serve_uri, _bundle} = ProviderBundle.resolve_serve_target(root)
    assert serve_uri =~ "args="

    {:ok, state} = Exec.init(serve_uri)

    {:noreply, state} =
      Exec.handle_message("hello", <<1::256>>, %{request_id: <<1::128>>, framed?: true}, state)

    line =
      receive do
        {port, {:data, {:eol, line}}} when port == state.port -> line
      after
        2_000 -> flunk("expected provider reply from exec runtime")
      end

    assert {:emit, [event], _state} = Exec.handle_info({state.port, {:data, {:eol, line}}}, state)
    assert event.body =~ "provider hello"
  end

  defp tmp_dir(prefix) do
    path = Path.join(System.tmp_dir!(), "#{prefix}_#{System.unique_integer([:positive])}")

    on_exit(fn ->
      File.rm_rf(path)
    end)

    path
  end
end
