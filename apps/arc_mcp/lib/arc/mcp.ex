defmodule Arc.MCP do
  @moduledoc """
  ARC's Model Context Protocol surface.
  """

  @protocol_version "2025-11-25"
  @server_name "arc-mcp"
  @server_version "0.1.0"

  @doc "Current MCP protocol version exposed by ARC."
  def protocol_version, do: @protocol_version

  @doc "Supported MCP protocol versions, newest first."
  def supported_protocol_versions do
    ["2025-11-25", "2025-06-18", "2025-03-26"]
  end

  @doc "Server name reported during MCP initialization."
  def server_name, do: @server_name

  @doc "Server version reported during MCP initialization."
  def server_version, do: @server_version

  @doc false
  def hello do
    :world
  end
end
