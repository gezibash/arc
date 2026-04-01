defmodule Arc.CLI.TrustStoreTest do
  use ExUnit.Case, async: false

  alias Arc.CLI.TrustStore
  alias Arc.Identity

  setup do
    trust_dir =
      Path.join(System.tmp_dir!(), "arc_trust_store_#{System.unique_integer([:positive])}")

    old_trust_dir = Application.get_env(:arc_cli, :trust_store_dir)
    Application.put_env(:arc_cli, :trust_store_dir, trust_dir)

    on_exit(fn ->
      if old_trust_dir do
        Application.put_env(:arc_cli, :trust_store_dir, old_trust_dir)
      else
        Application.delete_env(:arc_cli, :trust_store_dir)
      end

      File.rm_rf!(trust_dir)
    end)

    :ok
  end

  test "allow and deny are scoped by owner identity" do
    owner_one = Identity.generate()
    owner_two = Identity.generate()
    signer = Identity.encode_public_key(Identity.generate())

    assert {:ok, allowed} = TrustStore.allow(owner_one, signer)
    assert allowed["state"] == "allowed"

    assert {:ok, denied} = TrustStore.deny(owner_two, signer)
    assert denied["state"] == "denied"

    assert {:ok, %{"state" => "allowed"}} = TrustStore.get(owner_one, signer)
    assert {:ok, %{"state" => "denied"}} = TrustStore.get(owner_two, signer)
  end
end
