defmodule Arc.CLITest do
  use ExUnit.Case

  test "help output includes commands" do
    output = ExUnit.CaptureIO.capture_io(fn -> Arc.CLI.main(["help"]) end)
    assert output =~ "keys"
    assert output =~ "publish"
    assert output =~ "resolve"
    assert output =~ "install"
    assert output =~ "apps"
    assert output =~ "host"
    assert output =~ "tool"
    assert output =~ "info"
    assert output =~ "mcp"
    assert output =~ "version"
  end

  test "version prints the umbrella version and the build commit" do
    output = ExUnit.CaptureIO.capture_io(fn -> Arc.CLI.main(["version"]) end)
    assert output =~ ~r/^arc 0\.2\.0 \([0-9a-f]{7,}|unknown\)\n$/
    assert ExUnit.CaptureIO.capture_io(fn -> Arc.CLI.main(["--version"]) end) == output
  end
end
