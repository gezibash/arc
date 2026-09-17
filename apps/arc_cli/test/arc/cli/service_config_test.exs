defmodule Arc.CLI.ServiceConfigTest do
  use ExUnit.Case, async: true

  alias Arc.CLI.Application

  @publisher String.duplicate("a", 64)

  test "accepts an explicit relay service with a local update source" do
    assert {:ok, config} =
             Application.normalize_service_config(%{
               "role" => "relay",
               "key" => "operator",
               "port" => 7331,
               "update" => %{
                 "state_dir" => "/tmp/arc-service-state",
                 "publisher" => @publisher,
                 "pin" => :null,
                 "source" => %{"local_dir" => "/tmp/arc-releases"}
               }
             })

    assert config.role == :relay
    assert config.port == 7331
    assert config.update.channel == "stable"
    assert config.update.pin == nil
    assert config.update.source == %{local_dir: "/tmp/arc-releases"}
  end

  test "requires an explicit, pinned relay for ARC update transport" do
    assert {:ok, config} =
             Application.normalize_service_config(%{
               "role" => "relay",
               "key" => "operator",
               "port" => 7331,
               "update" => %{
                 "state_dir" => "/tmp/arc-service-state",
                 "publisher" => @publisher,
                 "channel" => "beta",
                 "source" => %{
                   "uri" => "releases+arc://#{@publisher}/releases",
                   "relay" => "127.0.0.1:7331",
                   "relay_pubkey" => @publisher
                 }
               }
             })

    assert config.update.source.relay == "127.0.0.1:7331"

    assert {:error, :invalid_service} =
             Application.normalize_service_config(%{
               "role" => "relay",
               "key" => "operator",
               "port" => 7331,
               "update" => %{
                 "state_dir" => "/tmp/arc-service-state",
                 "publisher" => @publisher,
                 "source" => %{"uri" => "release+arc://provider/releases"}
               }
             })
  end

  test "rejects unknown service and update settings" do
    assert {:error, :unknown_field} =
             Application.normalize_service_config(%{"role" => "relay", "extra" => true})

    assert {:error, :invalid_service} =
             Application.normalize_service_config(%{
               "role" => "relay",
               "key" => "operator",
               "port" => 7331,
               "update" => %{
                 "state_dir" => "/tmp/arc-service-state",
                 "publisher" => @publisher,
                 "source" => %{"local_dir" => "/tmp/arc-releases"},
                 "restart" => true
               }
             })
  end
end
