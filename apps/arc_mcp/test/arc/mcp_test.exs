defmodule Arc.MCPTest do
  use ExUnit.Case
  doctest Arc.MCP

  test "greets the world" do
    assert Arc.MCP.hello() == :world
  end
end
