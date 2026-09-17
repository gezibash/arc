defmodule Arc.CLI.Update.ManagerTest do
  use ExUnit.Case, async: false

  alias Arc.CLI.Update.{Manager, Store}

  setup do
    root =
      Path.join(
        System.tmp_dir!(),
        "arc-update-manager-" <> Base.encode16(:crypto.strong_rand_bytes(12))
      )

    File.mkdir!(root)
    on_exit(fn -> File.rm_rf!(root) end)

    config = %{
      update: %{
        state_dir: root,
        channel: "stable",
        publisher: String.duplicate("a", 64),
        pin: nil,
        source: %{local_dir: root}
      }
    }

    %{root: root, config: config}
  end

  test "each interrupted durable mutation phase blocks work after service restart", context do
    for phase <- ["staging", "applying", "observing", "committing"] do
      :ok = Store.write(context.root, "journal", %{"state" => phase})
      {:ok, manager} = Manager.start_link(config: context.config)

      assert {:ok, %{"state" => "blocked", "reconciliation_required" => true}} =
               Manager.request("status", %{})

      assert {:error, :reconciliation_required} = Manager.request("apply", %{})
      assert {:error, :reconciliation_required} = Manager.request("check", %{})
      GenServer.stop(manager)
    end
  end

  test "worker failure retains mutation evidence and fails closed across restart", context do
    journal = %{
      "state" => "observing",
      "from" => "base-build",
      "to" => "candidate-build",
      "target_version" => "0.3.3",
      "root" => "/private/managed-release",
      "archive_sha256" => String.duplicate("b", 64),
      "recorded_at" => 123
    }

    :ok = Store.write(context.root, "journal", journal)
    {:ok, manager} = Manager.start_link(config: context.config)
    ref = make_ref()
    test_pid = self()

    :sys.replace_state(Manager, fn state ->
      %{state | job: {test_pid, ref}, status: %{"state" => "observing"}}
    end)

    send(Manager, {:DOWN, ref, :process, test_pid, :killed})

    assert {:ok, %{"state" => "blocked", "reason" => "update_worker_failed"}} =
             Manager.request("status", %{})

    assert {:ok, persisted} = Store.read(context.root, "journal")
    assert persisted["interrupted_phase"] == "observing"
    assert persisted["from"] == "base-build"
    assert persisted["to"] == "candidate-build"
    assert persisted["target_version"] == "0.3.3"
    assert persisted["archive_sha256"] == String.duplicate("b", 64)

    GenServer.stop(manager)
    {:ok, restarted} = Manager.start_link(config: context.config)

    assert {:ok, status} = Manager.request("status", %{})
    assert status["reconciliation_required"] == true
    refute Map.has_key?(status, "root")
    GenServer.stop(restarted)
  end

  test "preflight failure remains retryable while a post-mutation failure blocks", context do
    {:ok, manager} = Manager.start_link(config: context.config)
    test_pid = self()

    :ok = Store.write(context.root, "journal", %{"state" => "checking"})
    preflight_ref = make_ref()

    :sys.replace_state(Manager, fn state ->
      %{state | job: {test_pid, preflight_ref}, status: %{"state" => "checking"}}
    end)

    send(Manager, {:update_result, test_pid, {:error, :release_preflight_failed}})

    assert {:ok, %{"state" => "blocked", "reconciliation_required" => false}} =
             Manager.request("status", %{})

    mutation = %{
      "state" => "applying",
      "from" => "base-build",
      "to" => "candidate-build",
      "target_version" => "0.3.3"
    }

    :ok = Store.write(context.root, "journal", mutation)
    mutation_ref = make_ref()

    :sys.replace_state(Manager, fn state ->
      %{state | job: {test_pid, mutation_ref}, status: mutation}
    end)

    send(Manager, {:update_result, test_pid, {:error, :installation_requires_reconciliation}})

    assert {:ok, %{"state" => "blocked", "reconciliation_required" => true}} =
             Manager.request("status", %{})

    assert {:ok, persisted} = Store.read(context.root, "journal")
    assert persisted["interrupted_phase"] == "applying"
    assert persisted["from"] == "base-build"
    assert persisted["to"] == "candidate-build"
    GenServer.stop(manager)
  end

  test "failed journal persistence leaves prior mutation evidence fail-closed", context do
    journal = %{
      "state" => "applying",
      "from" => "base-build",
      "to" => "candidate-build",
      "target_version" => "0.3.3",
      "root" => "/private/managed-release"
    }

    :ok = Store.write(context.root, "journal", journal)
    {:ok, manager} = Manager.start_link(config: context.config)
    ref = make_ref()
    test_pid = self()

    :sys.replace_state(Manager, fn state ->
      %{state | job: {test_pid, ref}, status: journal}
    end)

    try do
      :ok = File.chmod(context.root, 0o500)
      send(Manager, {:DOWN, ref, :process, test_pid, :killed})

      assert {:ok,
              %{
                "state" => "blocked",
                "reason" => "update_state_write_failed",
                "reconciliation_required" => true
              }} = Manager.request("status", %{})
    after
      :ok = File.chmod(context.root, 0o700)
    end

    assert {:ok, ^journal} = Store.read(context.root, "journal")
    GenServer.stop(manager)
    {:ok, restarted} = Manager.start_link(config: context.config)

    assert {:ok, %{"state" => "blocked", "reconciliation_required" => true}} =
             Manager.request("status", %{})

    GenServer.stop(restarted)
  end

  test "a corrupted journal during a worker result fails closed", context do
    {:ok, manager} = Manager.start_link(config: context.config)
    ref = make_ref()
    test_pid = self()

    :sys.replace_state(Manager, fn state ->
      %{state | job: {test_pid, ref}, status: %{"state" => "checking"}}
    end)

    File.write!(Path.join(context.root, "journal.json"), "not json")
    send(Manager, {:update_result, test_pid, {:error, :release_preflight_failed}})

    assert {:ok,
            %{
              "state" => "blocked",
              "reason" => "update_state_write_failed",
              "reconciliation_required" => true
            }} = Manager.request("status", %{})

    GenServer.stop(manager)
  end

  test "status is read-only and rejects arbitrary operations", context do
    start_supervised!({Manager, config: context.config})

    assert {:ok, %{"state" => "idle", "mode" => "notify", "busy" => false}} =
             Manager.request("status", %{})

    {:ok, status} = Manager.request("status", %{})
    assert :json.decode(IO.iodata_to_binary(:json.encode(status)))["pin"] == :null

    assert {:ok, %{}} = Store.read(context.root, "journal")
    assert {:error, :invalid_update_request} = Manager.request("eval", %{})
    assert {:error, :invalid_update_request} = Manager.request("apply", %{"force" => true})
  end

  test "corrupt persisted state prevents manager startup", context do
    Process.flag(:trap_exit, true)
    File.write!(Path.join(context.root, "journal.json"), "not json")
    assert {:error, :invalid_update_state} = Manager.start_link(config: context.config)
  end

  test "state files are private and invalid links are refused", context do
    :ok = Store.write(context.root, "checkpoint", %{"sequence" => 3})
    assert {:ok, %{"sequence" => 3}} = Store.read(context.root, "checkpoint")
    {:ok, stat} = File.stat(Path.join(context.root, "checkpoint.json"))
    assert Bitwise.band(stat.mode, 0o777) == 0o600

    File.ln_s!(
      Path.join(context.root, "checkpoint.json"),
      Path.join(context.root, "journal.json")
    )

    assert {:error, :invalid_update_state} = Store.read(context.root, "journal")
  end
end
