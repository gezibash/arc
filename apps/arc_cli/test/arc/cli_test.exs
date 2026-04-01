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
  end
end
