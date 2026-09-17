#!/usr/bin/env elixir

# Full, disposable proof for the managed native relay update path. The shared
# fixture lives beside this wrapper so other lifecycle proofs exercise exactly
# the same release construction and operator surface.
Code.require_file("support/managed_update_fixture.exs", __DIR__)

case System.argv() do
  [] -> Arc.ManagedUpdate.Proof.run()
  ["--federation"] -> Arc.ManagedUpdate.Proof.run_federation()
  _ -> raise("usage: mix run --no-start scripts/test-managed-update.exs [--federation]")
end
