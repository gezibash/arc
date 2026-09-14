defmodule Arc.CLI.Keys do
  @moduledoc """
  CLI commands for key management.

  Usage:
    arc keys gen              Generate a new key
    arc keys ls               List all keys
    arc keys use <name>       Set the active key
    arc keys show             Show the resolved active key
    arc keys rm <name>        Remove a key
  """

  alias Arc.Identity
  alias Arc.Identity.KeyStore

  def run(["gen" | _opts]) do
    case KeyStore.generate() do
      {:ok, id} ->
        IO.puts("Key generated")
        print_identity(id)
        IO.puts("")

        # Auto-set as default if it's the only key
        case KeyStore.list() do
          [{_, _}] ->
            :ok = KeyStore.set_default(Identity.name(id))
            IO.puts("  (set as default — only key)")

          _ ->
            IO.puts("  tip: run 'arc keys use #{Identity.short_name(id)}' to make it active")
        end
    end
  end

  def run(["ls" | _opts]) do
    keys = KeyStore.list()

    if keys == [] do
      IO.puts("No keys. Run 'arc keys gen' to create one.")
    else
      active = resolve_active_name()

      for {name, id} <- keys do
        marker = if name == active, do: "* ", else: "  "
        pk_hex = Identity.encode_public_key(id)
        pk_short = binary_part(pk_hex, 0, 4) <> "…" <> binary_part(pk_hex, byte_size(pk_hex), -4)
        IO.puts("#{marker}#{name}  (#{pk_short})")
      end

      if active && System.get_env("ARC_KEY") do
        IO.puts("\n  active via ARC_KEY=#{System.get_env("ARC_KEY")}")
      end
    end
  end

  def run(["use", name | _opts]) do
    case KeyStore.set_default(name) do
      :ok ->
        {:ok, id} = KeyStore.get(name)
        IO.puts("Active key: #{Identity.name(id)}")

      {:error, :not_found} ->
        error("no key matching '#{name}'")

      {:error, :ambiguous} ->
        error("'#{name}' matches multiple keys — be more specific")
    end
  end

  def run(["show" | _opts]) do
    case KeyStore.resolve_active() do
      {:ok, id} ->
        IO.puts("Active identity")
        print_identity(id)
        print_source()

      {:error, :no_default} ->
        error("No active key. Run 'arc keys gen' to create one.")

      {:error, :not_found} ->
        error("ARC_KEY='#{System.get_env("ARC_KEY")}' not found in key store")

      {:error, :ambiguous} ->
        error("ARC_KEY='#{System.get_env("ARC_KEY")}' matches multiple keys")
    end
  end

  def run(["rm", name | _opts]) do
    case KeyStore.get(name) do
      {:ok, id} ->
        full_name = Identity.name(id)
        :ok = KeyStore.remove(full_name)
        IO.puts("Removed: #{full_name}")

      {:error, :not_found} ->
        error("no key matching '#{name}'")

      {:error, :ambiguous} ->
        error("'#{name}' matches multiple keys — be more specific")
    end
  end

  def run(_) do
    IO.puts("""
    arc keys — manage ARC keys

    Commands:
      gen              Generate a new key
      ls               List all keys (* marks active)
      use <name>       Set the active key
      show             Show the resolved active key
      rm <name>        Remove a key

    Environment:
      ARC_KEY=<name>   Override active key for this terminal
    """)
  end

  defp resolve_active_name do
    case System.get_env("ARC_KEY") do
      nil ->
        case KeyStore.default_name() do
          {:ok, name} -> name
          _ -> nil
        end

      env_name ->
        case KeyStore.get(env_name) do
          {:ok, id} -> Identity.name(id)
          _ -> nil
        end
    end
  end

  defp print_identity(%Identity{} = id) do
    IO.puts("  name:       #{Identity.name(id)}")
    IO.puts("  public_key: #{Identity.encode_public_key(id)}")
  end

  defp print_source do
    if System.get_env("ARC_KEY") do
      IO.puts("  source:     ARC_KEY=#{System.get_env("ARC_KEY")}")
    end
  end

  @spec error(String.t()) :: no_return()
  defp error(msg) do
    IO.puts(:stderr, "error: #{msg}")
    Arc.CLI.Exit.halt(1)
  end
end
