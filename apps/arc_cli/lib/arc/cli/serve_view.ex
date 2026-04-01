defmodule Arc.CLI.ServeView do
  @moduledoc """
  Render serve startup banners and live request logs.
  """

  alias Arc.CLI.ToolRegistry
  alias Arc.Identity

  def render_banner(%Identity{} = identity, serve_meta, relay_info) when is_map(serve_meta) do
    capability = Map.get(serve_meta, :capability, %{})
    title = capability["title"] || capability["id"] || "ARC capability"
    kind = capability["kind"] || "capability"
    scheme = capability["scheme"] || serve_meta[:scheme] || "unknown"
    provider_name = Identity.name(identity)

    lines =
      [
        "Serving #{title} [#{kind}/#{scheme}]",
        "Identity: #{provider_name} (#{short_public_key(identity.public_key)})",
        "Short name: #{Identity.short_name(identity)}",
        relay_line(relay_info),
        optional_line("Bundle", serve_meta[:bundle_root]),
        optional_line("Runtime", serve_meta[:runtime_path]),
        optional_line("Manifest", serve_meta[:manifest_path]),
        "Inspect: arc info #{provider_name} primary",
        "Install: arc install #{provider_name} primary"
      ]
      |> Enum.reject(&is_nil/1)

    command_lines =
      case Map.get(serve_meta, :command_usages) || command_usages(capability) do
        [] -> []
        usages -> ["Use:"] ++ Enum.map(usages, &("  " <> &1))
      end

    log_lines = [
      "Logs: incoming requests will appear below",
      "Press Ctrl+C to stop."
    ]

    Enum.join(lines ++ [""] ++ command_lines ++ if(command_lines == [], do: [], else: [""]) ++ log_lines, "\n")
  end

  def render_event(event) when is_map(event) do
    peer = event[:from] || "unknown"
    key = event[:peer_key] || ""

    prefix =
      ["[serve]", label_for(event[:type]), "from", peer_with_key(peer, key)]
      |> Enum.reject(&(&1 in [nil, ""]))
      |> Enum.join(" ")

    suffix =
      case event[:type] do
        :request ->
          join_parts([
            event[:method] || "RAW",
            event[:path] || "/",
            preview_label(event[:body])
          ])

        :stream_open ->
          join_parts([
            event[:method] || "RAW",
            event[:path] || "/",
            "session=" <> to_string(event[:app_session_id] || "-"),
            preview_label(event[:body])
          ])

        :stream_data ->
          join_parts([
            "session=" <> to_string(event[:app_session_id] || "-"),
            "#{event[:bytes] || 0}B",
            preview_label(event[:body])
          ])

        :stream_resize ->
          join_parts([
            "session=" <> to_string(event[:app_session_id] || "-"),
            "#{event[:cols] || "?"}x#{event[:rows] || "?"}"
          ])

        :stream_close ->
          join_parts([
            "session=" <> to_string(event[:app_session_id] || "-")
          ])

        _ ->
          preview_label(event[:body])
      end

    String.trim(prefix <> " " <> suffix)
  end

  def command_usages(capability) when is_map(capability) do
    namespace = get_in(capability, ["interfaces", "cli", "namespace"])

    cond do
      !is_binary(namespace) or namespace == "" ->
        []

      ToolRegistry.subcommands(capability) != [] ->
        capability
        |> ToolRegistry.subcommands()
        |> Enum.map(&ToolRegistry.command_usage(namespace, &1))

      true ->
        [ToolRegistry.default_usage(namespace, capability)]
    end
  end

  def command_usages(_capability), do: []

  defp label_for(:request), do: "request"
  defp label_for(:stream_open), do: "stream-open"
  defp label_for(:stream_data), do: "stream-data"
  defp label_for(:stream_resize), do: "stream-resize"
  defp label_for(:stream_close), do: "stream-close"
  defp label_for(other), do: to_string(other || "event")

  defp relay_line(nil), do: "Relay: local only"

  defp relay_line(%{host: host, port: port, connected?: true} = relay) do
    pin =
      case Map.get(relay, :pubkey_pin) do
        nil -> "unpin"
        pubkey -> short_public_key(pubkey)
      end

    "Relay: #{host}:#{port} (pin #{pin})"
  end

  defp relay_line(%{host: host, port: port, error: reason}) do
    "Relay: #{host}:#{port} (connect failed: #{inspect(reason)})"
  end

  defp optional_line(_label, nil), do: nil
  defp optional_line(_label, ""), do: nil
  defp optional_line(label, value), do: "#{label}: #{value}"

  defp preview_label(nil), do: nil
  defp preview_label(""), do: nil

  defp preview_label(body) do
    sanitized =
      body
      |> to_string()
      |> String.replace(~r/\s+/u, " ")
      |> String.trim()

    if sanitized == "" do
      nil
    else
      inspect(String.slice(sanitized, 0, 72))
    end
  end

  defp peer_with_key(peer, ""), do: peer
  defp peer_with_key(peer, key), do: peer <> " (" <> key <> ")"

  defp join_parts(parts) do
    parts
    |> Enum.reject(&(&1 in [nil, ""]))
    |> Enum.join(" ")
  end

  defp short_public_key(binary) when is_binary(binary) and byte_size(binary) > 0 do
    pk_hex = Base.encode16(binary, case: :lower)
    binary_part(pk_hex, 0, 4) <> "…" <> binary_part(pk_hex, byte_size(pk_hex), -4)
  end

  defp short_public_key(_), do: "unknown"
end
