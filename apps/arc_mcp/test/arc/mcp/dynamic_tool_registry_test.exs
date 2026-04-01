defmodule Arc.MCP.DynamicToolRegistryTest do
  use ExUnit.Case, async: false

  alias Arc.Identity
  alias Arc.MCP.DynamicToolRegistry

  setup do
    dir =
      Path.join(
        System.tmp_dir!(),
        "arc_mounts_#{System.unique_integer([:positive, :monotonic])}"
      )

    File.mkdir_p!(dir)

    on_exit(fn ->
      File.rm_rf!(dir)
    end)

    %{dir: dir}
  end

  test "mount stores one capability per task and lists it", %{dir: dir} do
    owner = Identity.generate()
    detail = detail_doc("alpha", "primary")

    assert {:ok, mount} = DynamicToolRegistry.mount(owner, "demo", detail, dir: dir)
    assert mount["mount_id"] == "alpha/primary"
    assert mount["owner_name"] == Identity.name(owner)

    assert {:ok, mounts} = DynamicToolRegistry.list(owner, "demo", dir: dir)
    assert length(mounts) == 1
    assert hd(mounts)["capability"]["id"] == "primary"
  end

  test "get returns one mounted capability by provider and id", %{dir: dir} do
    owner = Identity.generate()

    assert {:ok, _} = DynamicToolRegistry.mount(owner, "demo", detail_doc("alpha", "primary"), dir: dir)

    assert {:ok, mount} = DynamicToolRegistry.get(owner, "demo", "alpha", "primary", dir: dir)
    assert mount["mount_id"] == "alpha/primary"
  end

  test "mount enforces the working-set cap", %{dir: dir} do
    owner = Identity.generate()

    assert {:ok, _} =
             DynamicToolRegistry.mount(owner, "demo", detail_doc("alpha", "one"), dir: dir, limit: 2)

    assert {:ok, _} =
             DynamicToolRegistry.mount(owner, "demo", detail_doc("beta", "two"), dir: dir, limit: 2)

    assert {:error, {:mount_limit_exceeded, 2}} =
             DynamicToolRegistry.mount(owner, "demo", detail_doc("gamma", "three"), dir: dir, limit: 2)
  end

  test "unmount removes a mounted capability", %{dir: dir} do
    owner = Identity.generate()

    assert {:ok, _} = DynamicToolRegistry.mount(owner, "demo", detail_doc("alpha", "one"), dir: dir)
    assert :ok = DynamicToolRegistry.unmount(owner, "demo", "alpha", "one", dir: dir)
    assert {:ok, []} = DynamicToolRegistry.list(owner, "demo", dir: dir)
  end

  test "different owners get isolated toolboxes for the same task", %{dir: dir} do
    first_owner = Identity.generate()
    second_owner = Identity.generate()

    assert {:ok, _} =
             DynamicToolRegistry.mount(first_owner, "demo", detail_doc("alpha", "one"), dir: dir)

    assert {:ok, _} =
             DynamicToolRegistry.mount(second_owner, "demo", detail_doc("beta", "two"), dir: dir)

    assert {:ok, [first_mount]} = DynamicToolRegistry.list(first_owner, "demo", dir: dir)
    assert {:ok, [second_mount]} = DynamicToolRegistry.list(second_owner, "demo", dir: dir)

    assert first_mount["provider"]["name"] == "alpha"
    assert second_mount["provider"]["name"] == "beta"
  end

  defp detail_doc(provider_name, capability_id) do
    %{
      "provider" => %{"name" => provider_name, "short_name" => provider_name},
      "capability" => %{
        "id" => capability_id,
        "kind" => "service",
        "scheme" => "http",
        "title" => "HTTP Proxy",
        "summary" => "Proxy HTTP requests",
        "invocation" => %{"method" => "RAW", "path" => "/"}
      }
    }
  end
end
