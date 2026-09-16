defmodule Arc.CLITest do
  use ExUnit.Case

  setup do
    lists_dir =
      Path.join(System.tmp_dir!(), "arc_cli_lists_#{System.unique_integer([:positive])}")

    old_lists_dir = Application.get_env(:arc_cli, :lists_dir)
    Application.put_env(:arc_cli, :lists_dir, lists_dir)

    on_exit(fn ->
      if old_lists_dir do
        Application.put_env(:arc_cli, :lists_dir, old_lists_dir)
      else
        Application.delete_env(:arc_cli, :lists_dir)
      end

      File.rm_rf!(lists_dir)
    end)

    :ok
  end

  test "help output includes commands" do
    output = ExUnit.CaptureIO.capture_io(fn -> Arc.CLI.main(["help"]) end)
    assert output =~ "keys"
    assert output =~ "publish"
    assert output =~ "resolve"
    assert output =~ "install"
    assert output =~ "apps"
    assert output =~ "host"
    assert output =~ "status"
    assert output =~ "tool"
    assert output =~ "info"
    assert output =~ "mcp"
    assert output =~ "version"
  end

  test "an unknown command prints an error and exits 1 without stopping the VM" do
    {result, stderr} =
      ExUnit.CaptureIO.with_io(:stderr, fn -> Arc.CLI.main(["no-such-command"]) end)

    assert result == {:exit, 1}
    assert stderr =~ "unknown command: no-such-command"
  end

  test "federation opt-in flags are limited to live listeners and providers" do
    {result, stderr} =
      ExUnit.CaptureIO.with_io(:stderr, fn ->
        Arc.CLI.main(["discover", "files", "--federate"])
      end)

    assert result == {:exit, 1}
    assert stderr =~ "federation flags are only supported by `arc serve` and `arc listen`"

    {result, stderr} =
      ExUnit.CaptureIO.with_io(:stderr, fn ->
        Arc.CLI.main(["discover", "files", "--federate-network"])
      end)

    assert result == {:exit, 1}
    assert stderr =~ "federation flags are only supported by `arc serve` and `arc listen`"
  end

  test "direct and onward federation flags cannot be combined" do
    {result, stderr} =
      ExUnit.CaptureIO.with_io(:stderr, fn ->
        Arc.CLI.main(["listen", "--federate", "--federate-network"])
      end)

    assert result == {:exit, 1}
    assert stderr =~ "--federate and --federate-network cannot be combined"
  end

  test "a direct policy is rejected before resolving a serve target" do
    policy =
      Path.join(
        System.tmp_dir!(),
        "arc-invalid-direct-policy-#{System.unique_integer([:positive])}.json"
      )

    File.write!(policy, "not json")

    on_exit(fn -> File.rm(policy) end)

    {result, stderr} =
      ExUnit.CaptureIO.with_io(:stderr, fn ->
        Arc.CLI.main([
          "serve",
          "/does/not/exist",
          "--direct-policy",
          policy,
          "--relay",
          "127.0.0.1:7331",
          "--relay-pubkey",
          String.duplicate("0", 64)
        ])
      end)

    assert result == {:exit, 1}
    assert stderr =~ "invalid direct policy"
    refute stderr =~ "Arcfile"
  end

  test "lists add, ls, and rm keep a peer list per tool" do
    run = fn argv -> ExUnit.CaptureIO.capture_io(fn -> Arc.CLI.main(argv) end) end

    assert run.(["lists", "add", "dm", "devs", "alice", "bob"]) == "dm/devs: alice bob\n"
    assert run.(["lists", "add", "dm", "devs", "bob", "carol"]) == "dm/devs: alice bob carol\n"
    assert run.(["lists", "ls", "dm"]) == "dm/devs\n"
    assert run.(["lists", "ls", "dm", "devs"]) == "alice\nbob\ncarol\n"
    assert Arc.CLI.Lists.expand("dm", "devs") == ["alice", "bob", "carol"]
    assert run.(["lists", "rm", "dm", "devs", "bob"]) == "dm/devs: alice carol\n"
    assert run.(["lists", "rm", "dm", "devs"]) == "removed dm/devs\n"
    assert is_nil(Arc.CLI.Lists.expand("dm", "devs"))
  end

  test "cache stores opened records sealed and searches them" do
    id = Arc.Identity.generate()
    :ok = Arc.Identity.KeyStore.save(id)
    System.put_env("ARC_KEY", Arc.Identity.name(id))

    on_exit(fn ->
      System.delete_env("ARC_KEY")
      Arc.Identity.KeyStore.remove(Arc.Identity.name(id))
    end)

    run = fn argv -> ExUnit.CaptureIO.capture_io(fn -> Arc.CLI.main(argv) end) end
    peer = Arc.Identity.generate()
    hex = Arc.Identity.encode_public_key(peer)

    text =
      "#{hex} · 2 messages, 0 unread\n" <>
        "01AAAAAAAAAAAAAAAAAAAAAAAA\tin\t#{hex}\t2026-09-14T19:22:50Z\t-\tread\tthe relay cap is wrong\nsecond line\n" <>
        "01BBBBBBBBBBBBBBBBBBBBBBBB\tout\t#{hex}\t2026-09-14T19:23:00Z\t-\tread\tagreed"

    # Off by default: nothing stored.
    assert Arc.CLI.Cache.store("dm", id, text) == text
    assert run.(["cache", "status", "dm"]) == "cache off for dm, 0 records\n"

    assert run.(["cache", "on", "dm"]) == "cache on for dm\n"
    assert Arc.CLI.Cache.store("dm", id, text) == text
    assert run.(["cache", "status", "dm"]) == "cache on for dm, 2 records\n"

    # On disk it is ciphertext.
    [file | _] =
      Path.wildcard(Path.join(Application.fetch_env!(:arc_cli, :cache_dir), "dm/*/*.sealed"))

    refute File.read!(file) =~ "relay cap"

    out = run.(["cache", "search", "dm", "RELAY cap"])

    assert out ==
             "01AAAAAAAAAAAAAAAAAAAAAAAA\tin\t#{Arc.Identity.name(peer)}\t2026-09-14T19:22:50Z\tthe relay cap is wrong\n"

    assert run.(["cache", "search", "dm", "nothing here"]) == "no matches\n"

    assert run.(["cache", "clear", "dm"]) == "cache cleared for dm\n"
    assert run.(["cache", "status", "dm"]) == "cache off for dm, 0 records\n"
  end

  test "version prints the umbrella version and the build commit" do
    output = ExUnit.CaptureIO.capture_io(fn -> Arc.CLI.main(["version"]) end)
    version = Regex.escape(Mix.Project.config()[:version])
    assert output =~ ~r/^arc #{version} \([0-9a-f]{7,}|unknown\)\n$/
    assert ExUnit.CaptureIO.capture_io(fn -> Arc.CLI.main(["--version"]) end) == output
  end
end
