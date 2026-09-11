defmodule Arc.MCP.ToolProjection do
  @moduledoc """
  Project mounted ARC capabilities into MCP tool descriptors.

  Only capabilities mounted for one owner identity and task are exposed, keeping
  the tool surface bounded to the current working set. A small ARC-native base
  toolset stays available regardless of mounts.
  """

  alias Arc.Data.Agent
  alias Arc.Data.CapabilityInvocation
  alias Arc.Data.Frame
  alias Arc.MCP.DynamicToolRegistry

  @type descriptor :: %{
          mount: map(),
          mount_id: String.t(),
          tool: map()
        }

  @default_send_timeout_ms 2_000

  @spec list(binary(), String.t(), keyword()) :: {:ok, [descriptor()]} | {:error, term()}
  def list(owner, task, opts \\ []) when is_binary(task) and task != "" do
    with {:ok, mounts} <- DynamicToolRegistry.list(owner, task, registry_opts(opts)) do
      {:ok, [send_descriptor() | Enum.map(mounts, &descriptor_from_mount/1)]}
    end
  end

  @spec lookup(binary(), String.t(), String.t(), keyword()) ::
          {:ok, descriptor()} | {:error, :not_found | term()}
  def lookup(owner, task, tool_name, opts \\ [])
      when is_binary(task) and task != "" and is_binary(tool_name) do
    with {:ok, descriptors} <- list(owner, task, opts) do
      case Enum.find(descriptors, &(&1.tool["name"] == tool_name)) do
        nil -> {:error, :not_found}
        descriptor -> {:ok, descriptor}
      end
    end
  end

  @spec fingerprint(binary(), String.t(), keyword()) :: {:ok, binary()} | {:error, term()}
  def fingerprint(owner, task, opts \\ []) when is_binary(task) and task != "" do
    with {:ok, descriptors} <- list(owner, task, opts) do
      payload =
        descriptors
        |> Enum.map(& &1.tool)
        |> Enum.sort_by(& &1["name"])
        |> :json.encode()
        |> IO.iodata_to_binary()

      {:ok, :crypto.hash(:sha256, payload)}
    end
  end

  @spec call(pid(), binary(), String.t(), String.t(), map(), keyword()) ::
          {:ok, map()} | {:error, term()}
  def call(agent, owner, task, tool_name, arguments, opts \\ [])
      when is_pid(agent) and is_binary(task) and is_binary(tool_name) and is_map(arguments) do
    case tool_name do
      "send" ->
        call_send_tool(agent, arguments)

      _ ->
        with {:ok, descriptor} <- lookup(owner, task, tool_name, opts),
             {:ok, input} <- extract_input(arguments),
             {:ok, reply} <-
               CapabilityInvocation.invoke(agent, descriptor.mount, input,
                 app_session_id: extract_app_session_id(arguments)
               ) do
          {:ok, success_result(reply.text)}
        else
          {:error, :not_found} ->
            {:error, {:tool_not_found, tool_name}}

          {:error, :missing_input} ->
            {:error, {:invalid_arguments, "expected arguments.input to be a string"}}

          {:error, {:remote, code, message}} ->
            {:error, {:tool_error, "#{code}: #{message}"}}

          {:error, reason} ->
            {:error, {:tool_error, inspect(reason)}}
        end
    end
  end

  @spec error_result(String.t()) :: map()
  def error_result(message) when is_binary(message) do
    %{
      "content" => [%{"type" => "text", "text" => message}],
      "isError" => true
    }
  end

  defp descriptor_from_mount(mount) do
    provider = mount["provider"] || %{}
    capability = mount["capability"] || %{}
    mount_id = mount["mount_id"] || mount_id(provider, capability)
    tool_name = tool_name(provider, capability)
    title = capability["title"] || capability["id"] || tool_name

    %{
      mount: mount,
      mount_id: mount_id,
      tool: %{
        "name" => tool_name,
        "title" => title,
        "description" => mount_description(provider, capability),
        "inputSchema" => mount_input_schema(capability)
      }
    }
  end

  defp mount_description(provider, capability) do
    summary = capability["summary"] || ""
    provider_name = provider["name"] || provider["short_name"] || "unknown"
    capability_id = capability["id"] || "unknown"
    kind = capability["kind"] || "capability"
    scheme = capability["scheme"] || "unknown"

    [summary, "Mounted from #{provider_name}/#{capability_id} [#{kind}/#{scheme}]"]
    |> Enum.reject(&(&1 in [nil, ""]))
    |> Enum.join(" ")
  end

  defp mount_input_schema(capability) do
    request_body = get_in(capability, ["invocation", "request_body"]) || %{}

    %{
      "type" => "object",
      "properties" => %{
        "input" => %{
          "type" => "string",
          "description" =>
            request_body["description"] || "Capability input payload to send over ARC"
        },
        "app_session_id" => %{
          "type" => "string",
          "description" =>
            "Optional ARC app session id for capabilities that maintain per-session state"
        }
      },
      "required" => ["input"],
      "additionalProperties" => false
    }
  end

  defp send_descriptor do
    %{
      mount: nil,
      mount_id: "arc/send",
      tool: %{
        "name" => "send",
        "title" => "Send Direct Message",
        "description" =>
          "Send a direct ARC message from the authenticated identity to another ARC participant.",
        "inputSchema" => %{
          "type" => "object",
          "properties" => %{
            "to" => %{
              "type" => "string",
              "description" => "Recipient petname, short petname, or public-key prefix"
            },
            "message" => %{
              "type" => "string",
              "description" => "Direct message body to send over ARC"
            },
            "await_reply" => %{
              "type" => "boolean",
              "description" => "Wait briefly for one reply before returning",
              "default" => true
            },
            "timeout_ms" => %{
              "type" => "integer",
              "description" => "Reply wait timeout in milliseconds when await_reply is true",
              "minimum" => 1
            }
          },
          "required" => ["to", "message"],
          "additionalProperties" => false
        }
      }
    }
  end

  defp success_result(text) when is_binary(text) do
    base = %{"content" => [%{"type" => "text", "text" => text}], "isError" => false}

    case decode_structured(text) do
      {:ok, structured} -> Map.put(base, "structuredContent", structured)
      :error -> base
    end
  end

  defp decode_structured(text) do
    case :json.decode(text) do
      structured when is_map(structured) or is_list(structured) -> {:ok, structured}
      _ -> :error
    end
  rescue
    _ -> :error
  end

  defp extract_input(%{"input" => input}) when is_binary(input), do: {:ok, input}
  defp extract_input(_arguments), do: {:error, :missing_input}

  defp extract_app_session_id(%{"app_session_id" => sid}) when is_binary(sid) and sid != "",
    do: sid

  defp extract_app_session_id(_arguments), do: nil

  defp call_send_tool(agent, arguments) do
    with {:ok, to} <- extract_required_string(arguments, "to"),
         {:ok, message} <- extract_required_string(arguments, "message"),
         {:ok, _entry} <- Agent.connect(agent, to),
         request_id = Frame.new_request_id(),
         :ok <-
           Agent.send_message(agent, to, message,
             request_id: request_id,
             meta: %{"method" => "RAW", "path" => "/"}
           ) do
      {:ok, wait_for_send_reply(agent, to, request_id, arguments)}
    else
      {:error, {:missing_argument, name}} ->
        {:error, {:invalid_arguments, "expected arguments.#{name} to be a string"}}

      {:error, :not_found} ->
        {:error, {:tool_error, "no identity found for recipient"}}

      {:error, :no_keyex} ->
        {:error, {:tool_error, "recipient has no key exchange material published"}}

      {:error, reason} ->
        {:error, {:tool_error, inspect(reason)}}
    end
  end

  defp wait_for_send_reply(agent, to, request_id, arguments) do
    if await_reply?(arguments) do
      timeout_ms = reply_timeout(arguments)

      case wait_for_reply(agent, request_id, timeout_ms) do
        {:ok, %{kind: :response, text: text}} -> success_result(text)
        {:ok, %{kind: :error, text: text}} -> error_result(text)
        {:ok, %{text: text}} -> success_result(text)
        {:error, :timeout} -> success_result("Sent to #{to}")
      end
    else
      success_result("Sent to #{to}")
    end
  end

  defp wait_for_reply(agent, request_id, timeout_ms) do
    started_at = System.monotonic_time(:millisecond)
    wait_for_reply(agent, request_id, timeout_ms, started_at)
  end

  defp wait_for_reply(agent, request_id, timeout_ms, started_at) do
    Process.sleep(100)
    Agent.poll_mailbox(agent)
    Process.sleep(10)

    case Agent.read_inbox(agent) do
      [] ->
        if elapsed_ms(started_at) > timeout_ms do
          {:error, :timeout}
        else
          wait_for_reply(agent, request_id, timeout_ms, started_at)
        end

      messages ->
        case Enum.split_with(messages, &(&1[:request_id] == request_id)) do
          {[], _unmatched} ->
            if elapsed_ms(started_at) > timeout_ms do
              {:error, :timeout}
            else
              wait_for_reply(agent, request_id, timeout_ms, started_at)
            end

          {[match | _], _unmatched} ->
            {:ok, match}
        end
    end
  end

  defp elapsed_ms(started_at) do
    System.monotonic_time(:millisecond) - started_at
  end

  defp await_reply?(%{"await_reply" => false}), do: false
  defp await_reply?(%{"await_reply" => _}), do: true
  defp await_reply?(_arguments), do: true

  defp reply_timeout(%{"timeout_ms" => timeout_ms})
       when is_integer(timeout_ms) and timeout_ms > 0,
       do: timeout_ms

  defp reply_timeout(_arguments), do: @default_send_timeout_ms

  defp extract_required_string(arguments, key) do
    case Map.get(arguments, key) do
      value when is_binary(value) and value != "" -> {:ok, value}
      _ -> {:error, {:missing_argument, key}}
    end
  end

  defp tool_name(provider, capability) do
    provider_name = provider["name"] || provider["short_name"] || "provider"
    capability_id = capability["id"] || "capability"
    "arc_" <> sanitize(provider_name) <> "__" <> sanitize(capability_id)
  end

  defp mount_id(provider, capability) do
    provider_name = provider["name"] || provider["short_name"] || "unknown"
    capability_id = capability["id"] || "unknown"
    provider_name <> "/" <> capability_id
  end

  defp sanitize(value) when is_binary(value) do
    sanitized =
      value
      |> String.downcase()
      |> String.replace(~r/[^a-z0-9]+/u, "_")
      |> String.trim("_")

    if sanitized == "", do: "tool", else: sanitized
  end

  defp registry_opts(opts), do: Keyword.get(opts, :registry_opts, opts)
end
