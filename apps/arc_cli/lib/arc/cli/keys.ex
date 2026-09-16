defmodule Arc.CLI.Keys do
  @moduledoc """
  CLI commands for key management.

  Usage:
    arc keys gen              Generate a new key
    arc keys ls               List all keys
    arc keys use <name>       Set the global default key
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
            case KeyStore.set_default(Identity.name(id)) do
              :ok -> IO.puts("  (set as default — only key)")
              {:error, reason} -> error(describe_error(reason))
            end

          _ ->
            IO.puts("  tip: run 'arc keys use #{Identity.short_name(id)}' to make it active")
        end
    end
  end

  def run(["ls" | _opts]) do
    keys = KeyStore.list()
    {active, source} = active_name_and_source()

    if keys == [] do
      IO.puts("No keys. Run 'arc keys gen' to create one.")
    else
      for {name, id} <- keys do
        marker = if name == active, do: "* ", else: "  "
        pk_hex = Identity.encode_public_key(id)
        pk_short = binary_part(pk_hex, 0, 4) <> "…" <> binary_part(pk_hex, byte_size(pk_hex), -4)
        IO.puts("#{marker}#{name}  (#{pk_short})")
      end

      if active, do: IO.puts("\n  active via #{source_label(source)}")
    end
  end

  def run(["use", name | _opts]) do
    case KeyStore.set_default(name) do
      :ok ->
        {:ok, id} = KeyStore.get(name)
        IO.puts("Default key: #{Identity.name(id)}")

      {:error, :not_found} ->
        error("no key matching '#{name}'")

      {:error, :ambiguous} ->
        error("'#{name}' matches multiple keys — be more specific")

      {:error, reason} ->
        error(describe_error(reason))
    end
  end

  def run(["show" | _opts]) do
    case KeyStore.resolve_active_with_source() do
      {:ok, id, source} ->
        IO.puts("Active identity")
        print_identity(id)
        IO.puts("  source:     #{source_label(source)}")

      {:error, reason} ->
        error(describe_error(reason))
    end
  end

  def run(["rm", name | _opts]) do
    case KeyStore.get(name) do
      {:ok, id} ->
        full_name = Identity.name(id)

        case KeyStore.remove(full_name) do
          :ok -> IO.puts("Removed: #{full_name}")
          {:error, reason} -> error(describe_error(reason))
        end

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
      use <name>       Set the global default in ~/.config/arc/default.key
      show             Show the resolved active key
      rm <name>        Remove a key

    Environment:
      ARC_KEY=<name>   Override active key for this terminal

    Selection order:
      ARC_KEY -> ./arc.key -> ~/.config/arc/default.key
      Files contain a key name or unambiguous prefix. No parent-directory search.
      An invalid selection is an error; only a missing selector falls through.
    """)
  end

  @doc false
  def describe_error(:no_default),
    do: "No active key. Run 'arc keys use <name>' to select one, or 'arc keys gen' to create one."

  def describe_error(:not_found),
    do:
      "Selected identity not found in key store; check ARC_KEY, ./arc.key, or the global default."

  def describe_error(:ambiguous),
    do: "Identity selector matches multiple keys; use a full key name."

  def describe_error({:invalid_identity_selector, source}),
    do:
      "Invalid identity selector in #{source_label(source)}; expected a non-empty key name or prefix."

  def describe_error({:identity_selector_file, path, reason}),
    do: "Could not access identity selector #{path}: #{:file.format_error(reason)}"

  def describe_error({:default_saved, reason}),
    do: "Default key was saved, but legacy selector cleanup failed. " <> describe_error(reason)

  def describe_error({:key_removed, reason}),
    do: "Key was removed, but default selector cleanup failed. " <> describe_error(reason)

  def describe_error(reason), do: inspect(reason)

  defp active_name_and_source do
    case KeyStore.resolve_active_with_source() do
      {:ok, id, source} -> {Identity.name(id), source}
      {:error, :no_default} -> {nil, nil}
      {:error, reason} -> error(describe_error(reason))
    end
  end

  defp source_label(:environment), do: "ARC_KEY"
  defp source_label({:file, path}), do: path

  defp print_identity(%Identity{} = id) do
    IO.puts("  name:       #{Identity.name(id)}")
    IO.puts("  public_key: #{Identity.encode_public_key(id)}")
  end

  @spec error(String.t()) :: no_return()
  defp error(msg) do
    IO.puts(:stderr, "error: #{msg}")
    Arc.CLI.Exit.halt(1)
  end
end
