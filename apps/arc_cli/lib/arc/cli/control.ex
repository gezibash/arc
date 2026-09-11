defmodule Arc.CLI.Control do
  @moduledoc """
  CLI commands for control plane operations.
  """

  alias Arc.Control
  alias Arc.Identity
  alias Arc.Identity.KeyStore

  def run(["publish" | _opts]) do
    with_identity(fn id ->
      case Control.publish(id) do
        :ok ->
          IO.puts("Published to control plane")
          IO.puts("  name:       #{Identity.name(id)}")
          IO.puts("  public_key: #{Identity.encode_public_key(id)}")

        {:error, reason} ->
          IO.puts(:stderr, "error: #{inspect(reason)}")
          System.halt(1)
      end
    end)
  end

  def run(["resolve", query | _opts]) do
    case Control.resolve(query) do
      {:ok, []} ->
        IO.puts("No identity found for '#{query}'")

      {:ok, entries} ->
        IO.puts("Found #{length(entries)} identity(s):\n")

        for entry <- entries do
          pk_hex = Base.encode16(entry.public_key, case: :lower)
          IO.puts("  name:       #{entry.name}")
          IO.puts("  public_key: #{pk_hex}")
          IO.puts("  keyex:      #{if entry.x25519_public, do: "published", else: "none"}")
          IO.puts("  status:     #{entry.status}")
          IO.puts("")
        end

      {:error, reason} ->
        IO.puts(:stderr, "error: #{inspect(reason)}")
        System.halt(1)
    end
  end

  def run(_) do
    IO.puts("""
    arc control plane commands

    Commands:
      publish               Publish current identity to the control plane
      resolve <query>       Resolve by petname, short name, or public key prefix
    """)
  end

  defp with_identity(fun) do
    case KeyStore.resolve_active() do
      {:ok, id} ->
        fun.(id)

      {:error, :no_default} ->
        IO.puts(:stderr, "No active key. Run 'arc keys gen' first.")
        System.halt(1)

      {:error, reason} ->
        IO.puts(:stderr, "error: #{inspect(reason)}")
        System.halt(1)
    end
  end
end
