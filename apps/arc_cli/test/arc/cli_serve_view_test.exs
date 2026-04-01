defmodule Arc.CLIServeViewTest do
  use ExUnit.Case, async: true

  alias Arc.CLI.ServeView
  alias Arc.Identity

  test "render_banner shows manifest identity relay and usage hints" do
    identity = Identity.generate()

    capability = %{
      "id" => "primary",
      "kind" => "data",
      "scheme" => "sqlite",
      "title" => "SQLite Database",
      "interfaces" => %{
        "cli" => %{
          "namespace" => "sqlite",
          "commands" => [
            %{
              "path" => [],
              "args" => [%{"name" => "sql", "kind" => "positional", "required" => true}]
            }
          ]
        }
      }
    }

    output =
      ServeView.render_banner(identity, %{capability: capability, manifest_path: "/tmp/manifest.json"}, %{
        host: "relay.arc",
        port: 7331,
        connected?: true,
        pubkey_pin: <<1::256>>
      })

    assert output =~ "Serving SQLite Database [data/sqlite]"
    assert output =~ "Identity: #{Identity.name(identity)}"
    assert output =~ "Short name: #{Identity.short_name(identity)}"
    assert output =~ "Relay: relay.arc:7331"
    assert output =~ "Manifest: /tmp/manifest.json"
    assert output =~ "Inspect: arc info #{Identity.name(identity)} primary"
    assert output =~ "Install: arc install #{Identity.name(identity)} primary"
    assert output =~ "arc sqlite <sql>"
    assert output =~ "Logs: incoming requests will appear below"
  end

  test "render_event formats request and stream events" do
    request =
      ServeView.render_event(%{
        type: :request,
        from: "peer-alpha",
        peer_key: "abcd…1234",
        method: "GET",
        path: "/info",
        body: ""
      })

    stream =
      ServeView.render_event(%{
        type: :stream_resize,
        from: "peer-beta",
        peer_key: "eeff…8899",
        app_session_id: "sb-12345678",
        cols: 120,
        rows: 40
      })

    assert request == "[serve] request from peer-alpha (abcd…1234) GET /info"
    assert stream == "[serve] stream-resize from peer-beta (eeff…8899) session=sb-12345678 120x40"
  end
end
