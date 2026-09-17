#!/usr/bin/env elixir

# Faults exist only in a recompiled Store inside disposable native fixtures.
# No production hook, live installation, user key store or distributed RPC.
Code.require_file("support/managed_update_fixture.exs", __DIR__)

defmodule Arc.UpdateRecovery.Proof do
  alias Arc.ManagedUpdate.Proof, as: Fixture

  @phases ~w(staging applying observing committing permanent current)

  def run do
    case System.argv() do
      [] ->
        Enum.each(@phases, &crash_at/1)
        corrupted_download()

      [phase] when phase in @phases ->
        crash_at(phase)

      _ ->
        raise("expected no arguments or one recovery phase")
    end

    IO.puts("update recovery proof: PASS")
  end

  defp fixture(fun, fault? \\ false) do
    root = Fixture.make_root()

    try do
      paths = Fixture.setup(root)
      Fixture.build_base(paths)
      if fault?, do: inject_store_barrier(paths)
      Fixture.prepare_base(paths)
      Fixture.build_candidate(paths)
      Fixture.build_package(paths)
      paths = Fixture.publish_channel(paths)
      fun.(paths)
    after
      Fixture.stop_service(Process.get(:recovery_service))
      Process.delete(:recovery_service)
      Fixture.remove_root(root)
    end
  end

  defp crash_at(phase) do
    fixture(
      fn paths ->
        boot = Path.join(paths.base, "releases/start_erl.data")
        original_boot = File.read!(boot)

        base_beam =
          Path.wildcard(Path.join(paths.base, "lib/arc_net-*/ebin/Elixir.Arc.Net.Relay.beam"))
          |> hd()

        original_beam = File.read!(base_beam)
        marker = Path.join(paths.root, "phase-reached")
        service = start(paths, phase, marker)
        ready(paths)

        request =
          Task.async(fn ->
            System.cmd(
              Path.join(paths.base, "bin/arc"),
              ["update", "apply", "--socket", Path.join(paths.state, "admin.sock")],
              stderr_to_stdout: true
            )
          end)

        wait(fn -> File.exists?(marker) end, "phase #{phase} was not reached", 30_000)
        ensure(File.read!(marker) == phase, "wrong phase barrier")

        journal = read_journal(paths)
        expected_phase = if phase == "permanent", do: "committing", else: phase
        ensure(journal["state"] == expected_phase, "phase was not durably recorded")

        # Kill only the owned child after its real write + directory sync finished.
        {:os_pid, pid} = Port.info(service, :os_pid)
        {_, 0} = System.cmd("kill", ["-KILL", Integer.to_string(pid)])
        await_exit(service)
        _ = Task.await(request, 10_000)
        Process.delete(:recovery_service)
        ensure(File.read!(base_beam) == original_beam, "previous release was overwritten")

        committed? = phase in ["permanent", "current"]

        if committed? do
          ensure(File.read!(boot) != original_boot, "committed boot selection was lost")
        else
          ensure(File.read!(boot) == original_boot, "uncommitted update changed boot selection")
        end

        # SIGKILL cannot remove a Unix socket. The operator must first establish
        # that its owning process has exited; this proof owns that exact child.
        socket = Path.join(paths.state, "admin.sock")
        ensure(match?({:ok, %{type: :other}}, File.lstat(socket)), "expected owned stale socket")
        :ok = File.rm(socket)
        _ = start(paths)
        Fixture.wait_socket(socket)

        if phase == "current" do
          status = Fixture.update(paths, "status")

          ensure(
            status["running"]["build"] == "managed-proof-target",
            "permanent release did not recover"
          )

          ensure(
            status["reconciliation_required"] != true,
            "completed update incorrectly blocked"
          )
        else
          status = Fixture.wait_status(paths, &(&1["state"] == "blocked"))
          expected_build = if committed?, do: "managed-proof-target", else: "managed-proof-base"

          ensure(
            status["running"]["build"] == expected_build,
            "committed release selection did not recover"
          )

          ensure(status["reconciliation_required"] == true, "interrupted update allowed retry")

          for operation <- ["check", "apply"] do
            {body, exit_status} =
              System.cmd(
                Path.join(paths.base, "bin/arc"),
                ["update", operation, "--socket", socket],
                stderr_to_stdout: true
              )

            ensure(
              exit_status != 0 and String.contains?(body, "reconciliation_required"),
              "interrupted update accepted #{operation}"
            )
          end

          # Let the automatic startup check fire: it must not undo the block.
          Process.sleep(1_100)

          ensure(
            Fixture.update(paths, "status")["reconciliation_required"] == true,
            "background check cleared recovery block"
          )

          ensure(
            File.read!(boot) != original_boot == committed?,
            "blocked restart changed release selection"
          )
        end

        relay = Fixture.relay_status(paths)
        expected_version = Fixture.update(paths, "status")["running"]["version"]

        ensure(
          relay["version"] == expected_version,
          "relay code disagrees with recovered release"
        )

        IO.puts("recovery #{phase}: PASS (boot preserved; operator policy enforced)")
      end,
      true
    )
  end

  defp corrupted_download do
    fixture(fn paths ->
      [blob] = Path.wildcard(Path.join(paths.provider, "blobs/*.tar.gz"))
      bytes = File.read!(blob)
      <<first, rest::binary>> = bytes
      File.write!(blob, <<Bitwise.bxor(first, 1), rest::binary>>)
      original_boot = File.read!(Path.join(paths.base, "releases/start_erl.data"))
      _ = start(paths)
      ready(paths)
      _ = Fixture.update(paths, "apply")
      blocked = Fixture.wait_status(paths, &(&1["state"] == "blocked"))

      ensure(
        blocked["reason"] == "digest_mismatch",
        "corrupt download was not rejected by digest"
      )

      ensure(
        blocked["reconciliation_required"] == false,
        "failed download requires mutation repair"
      )

      ensure(
        File.read!(Path.join(paths.base, "releases/start_erl.data")) == original_boot,
        "corrupt download changed boot selection"
      )

      ensure(
        not File.exists?(Path.join(paths.base, "releases/arc_runtime.tar.gz")),
        "corrupt download committed an archive"
      )

      # Restore the immutable provider bytes, then explicitly retry as operator.
      File.write!(blob, bytes)
      _ = Fixture.update(paths, "apply")
      status = Fixture.wait_status(paths, &(&1["state"] == "current"), 30_000)

      ensure(
        get_in(status, ["observation", "verdict"]) == "passed",
        "repaired download did not complete safely"
      )

      IO.puts("recovery corrupt download: PASS (rejected; explicit retry succeeded)")
    end)
  end

  defp start(paths, phase \\ "", marker \\ "") do
    service =
      Port.open({:spawn_executable, String.to_charlist(Path.join(paths.base, "bin/arc"))}, [
        :binary,
        :stderr_to_stdout,
        :exit_status,
        args: [~c"service", ~c"start", ~c"--config", String.to_charlist(paths.config)],
        env: [
          {~c"ARC_RECOVERY_PROOF_PHASE", String.to_charlist(phase)},
          {~c"ARC_RECOVERY_PROOF_MARKER", String.to_charlist(marker)}
        ]
      ])

    Process.put(:recovery_service, service)
    service
  end

  defp ready(paths) do
    Fixture.wait_socket(Path.join(paths.state, "admin.sock"))
    _ = Fixture.update(paths, "check")
    Fixture.wait_status(paths, &(&1["state"] == "available"))
  end

  defp inject_store_barrier(paths) do
    source = File.read!(Path.expand("../apps/arc_cli/lib/arc/cli/update/store.ex", __DIR__))
    ast = Code.string_to_quoted!(source)

    patched =
      Macro.postwalk(ast, fn
        {:def, meta, [{:when, _, [{:write, _, [_, _, document]}, _]} = head, [do: body]]} ->
          wrapped =
            quote do
              result = unquote(body)
              phase = System.get_env("ARC_RECOVERY_PROOF_PHASE")

              if result == :ok and phase != "" and unquote(document)["state"] == phase do
                File.write!(System.fetch_env!("ARC_RECOVERY_PROOF_MARKER"), phase)

                receive do
                  :never_sent_by_proof -> :ok
                end
              end

              result
            end

          {:def, meta, [head, [do: wrapped]]}

        node ->
          node
      end)

    ensure(patched != ast, "Store fault barrier was not inserted")
    file = Path.join(paths.root, "fault_store.ex")
    File.write!(file, Macro.to_string(patched))
    [ebin] = Path.wildcard(Path.join(paths.base, "lib/arc_cli-*/ebin"))
    arguments = Path.wildcard(Path.join(paths.base, "lib/*/ebin")) |> Enum.flat_map(&["-pa", &1])
    Fixture.command!(System.find_executable("elixirc"), arguments ++ ["-o", ebin, file])

    engine_source =
      File.read!(Path.expand("../apps/arc_cli/lib/arc/cli/update/engine.ex", __DIR__))

    engine_ast = Code.string_to_quoted!(engine_source)

    engine_patched =
      Macro.postwalk(engine_ast, fn
        {{:., _, [:release_handler, :make_permanent]}, _, [_]} = call ->
          quote do
            result = unquote(call)

            if result == :ok and System.get_env("ARC_RECOVERY_PROOF_PHASE") == "permanent" do
              File.write!(System.fetch_env!("ARC_RECOVERY_PROOF_MARKER"), "permanent")

              receive do
                :never_sent_by_proof -> :ok
              end
            end

            result
          end

        node ->
          node
      end)

    ensure(engine_patched != engine_ast, "commit fault barrier was not inserted")
    engine_file = Path.join(paths.root, "fault_engine.ex")
    File.write!(engine_file, Macro.to_string(engine_patched))
    Fixture.command!(System.find_executable("elixirc"), arguments ++ ["-o", ebin, engine_file])
  end

  defp read_journal(paths),
    do: paths.state |> Path.join("journal.json") |> File.read!() |> :json.decode()

  defp await_exit(port) do
    receive do
      {^port, {:exit_status, _}} -> :ok
      {^port, {:data, _}} -> await_exit(port)
    after
      5_000 -> raise("owned service did not exit")
    end
  end

  defp wait(predicate, message, remaining) when remaining > 0 do
    if predicate.() do
      :ok
    else
      Process.sleep(25)
      wait(predicate, message, remaining - 25)
    end
  end

  defp wait(_, message, _), do: raise(message)
  defp ensure(true, _), do: :ok
  defp ensure(false, message), do: raise(message)
end

Arc.UpdateRecovery.Proof.run()
