defmodule Arc.Host.Protocol do
  @moduledoc false

  alias Arc.Host.Token

  @version 1
  @transport "unix_socket_ndjson"
  @event_ops ["stream_data", "stream_exit", "stream_error"]

  def version, do: @version

  def describe do
    %{
      "version" => @version,
      "transport" => %{
        "kind" => @transport,
        "socket" => "unix",
        "encoding" => "json",
        "framing" => "newline-delimited"
      },
      "auth" => %{
        "initialize_requires" => "delegated_token",
        "admin_operations" => ["shutdown", "token.issue"],
        "delegated_scopes" => Token.delegated_scopes()
      },
      "operations" => operations(),
      "events" => Enum.map(@event_ops, &event_description/1)
    }
  end

  def operations do
    [
      op("status", "public"),
      op("protocol.describe", "public"),
      op("shutdown", "admin"),
      op("token.issue", "admin"),
      op("initialize", "delegated_token"),
      op("identity.current", "initialized"),
      op("resolve", "initialized", "resolve"),
      op("discover", "initialized", "discover"),
      op("info", "initialized", "info"),
      op("send", "initialized", "send"),
      op("call", "initialized", "call"),
      op("tool.list", "initialized", "tool.list"),
      op("tool.invoke", "initialized", "tool.invoke"),
      op("stream.open", "initialized", "stream"),
      op("stream.write", "initialized", "stream"),
      op("stream.resize", "initialized", "stream"),
      op("stream.close", "initialized", "stream")
    ]
  end

  defp op(name, auth), do: %{"name" => name, "auth" => auth}

  defp op(name, auth, scope), do: %{"name" => name, "auth" => auth, "scope" => scope}

  defp event_description("stream_data") do
    %{
      "name" => "stream_data",
      "fields" => ["op", "event", "app_session_id", "request_id", "from", "text", "meta"]
    }
  end

  defp event_description("stream_exit") do
    %{
      "name" => "stream_exit",
      "fields" => [
        "op",
        "event",
        "app_session_id",
        "request_id",
        "from",
        "text",
        "meta",
        "status"
      ]
    }
  end

  defp event_description("stream_error") do
    %{
      "name" => "stream_error",
      "fields" => ["op", "event", "app_session_id", "request_id", "from", "text", "meta"]
    }
  end
end
