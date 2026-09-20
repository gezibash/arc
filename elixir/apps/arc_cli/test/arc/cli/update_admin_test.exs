defmodule Arc.CLI.UpdateAdminTest do
  use ExUnit.Case, async: false

  alias Arc.CLI.Update.Admin
  alias Arc.CLI.Update.AdminClient

  defmodule Manager do
    def request("status", %{}), do: {:ok, %{"state" => "idle"}}
    def request("check", %{}), do: {:ok, %{"state" => "checking"}}
    def request("apply", %{}), do: {:error, :not_ready}
  end

  test "serves only bounded, named local update operations" do
    root = Path.join(System.tmp_dir!(), "arc-admin-#{System.unique_integer([:positive])}")
    on_exit(fn -> File.rm_rf(root) end)

    {:ok, admin} = Admin.start_link(state_dir: root, manager: Manager)
    socket = Path.join(root, "admin.sock")

    assert {:ok, %{"state" => "idle"}} = AdminClient.request(socket, "status")
    assert {:ok, %{"state" => "checking"}} = AdminClient.request(socket, "check")
    assert {:error, "not_ready"} = AdminClient.request(socket, "apply")
    assert File.stat!(socket).mode |> Bitwise.band(0o777) == 0o600

    GenServer.stop(admin)
    refute File.exists?(socket)
  end

  test "supervisor shutdown removes the socket so a service can restart" do
    root =
      Path.join(System.tmp_dir!(), "arc-admin-shutdown-#{System.unique_integer([:positive])}")

    on_exit(fn -> File.rm_rf(root) end)

    {:ok, supervisor} =
      Supervisor.start_link([{Admin, state_dir: root, manager: Manager}], strategy: :one_for_one)

    socket = Path.join(root, "admin.sock")
    assert File.exists?(socket)
    :ok = Supervisor.stop(supervisor)
    refute File.exists?(socket)
    {:ok, restarted} = Admin.start_link(state_dir: root, manager: Manager)
    assert {:ok, %{"state" => "idle"}} = AdminClient.request(socket, "status")
    GenServer.stop(restarted)
  end
end
