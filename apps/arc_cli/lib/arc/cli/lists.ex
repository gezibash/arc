defmodule Arc.CLI.Lists do
  @moduledoc """
  Saved peer lists, per tool namespace, on this machine.

    arc lists add <tool> <name> <peer>...   add peers to a list
    arc lists rm <tool> <name> [<peer>...]  remove peers, or the list
    arc lists ls <tool> [<name>]            show lists, or one list

  A list lives at `~/.config/arc/lists/<tool>/<name>`, one peer per line.
  When a command of that tool resolves peers, a value that names a list
  expands to its members.
  """

  @default_dir Path.join(["~", ".config", "arc", "lists"])
  @name ~r/^[a-z0-9][a-z0-9._-]*$/

  @doc "Members of list `name` for `namespace`, or nil when there is no such list."
  @spec expand(String.t(), String.t()) :: [String.t()] | nil
  def expand(namespace, name) when is_binary(namespace) and is_binary(name) do
    if Regex.match?(@name, name) do
      case File.read(path(namespace, name)) do
        {:ok, text} -> String.split(text, "\n", trim: true)
        _ -> nil
      end
    end
  end

  def run(["add", namespace, name | peers]) when peers != [] do
    with :ok <- valid_name(name) do
      members = Enum.uniq((expand(namespace, name) || []) ++ peers)
      write(namespace, name, members)
      IO.puts("#{namespace}/#{name}: #{Enum.join(members, " ")}")
    end
  end

  def run(["rm", namespace, name]) do
    with :ok <- valid_name(name) do
      File.rm(path(namespace, name))
      IO.puts("removed #{namespace}/#{name}")
    end
  end

  def run(["rm", namespace, name | peers]) do
    with :ok <- valid_name(name) do
      members = (expand(namespace, name) || []) -- peers
      write(namespace, name, members)
      IO.puts("#{namespace}/#{name}: #{Enum.join(members, " ")}")
    end
  end

  def run(["ls", namespace]) do
    case File.ls(Path.expand(Path.join(dir(), namespace))) do
      {:ok, names} -> Enum.each(Enum.sort(names), &IO.puts("#{namespace}/#{&1}"))
      _ -> IO.puts("no lists for #{namespace}")
    end
  end

  def run(["ls", namespace, name]) do
    case expand(namespace, name) do
      nil -> error("no list #{namespace}/#{name}")
      members -> Enum.each(members, &IO.puts/1)
    end
  end

  def run(_) do
    IO.puts("""
    arc lists commands

      lists add <tool> <name> <peer>...
      lists rm <tool> <name> [<peer>...]
      lists ls <tool> [<name>]
    """)
  end

  defp write(namespace, name, members) do
    file = path(namespace, name)
    File.mkdir_p!(Path.dirname(file))
    File.write!(file, Enum.map_join(members, "", &(&1 <> "\n")))
  end

  defp valid_name(name) do
    if Regex.match?(@name, name), do: :ok, else: error("invalid list name #{name}")
  end

  defp dir, do: Application.get_env(:arc_cli, :lists_dir, @default_dir)
  defp path(namespace, name), do: Path.expand(Path.join([dir(), namespace, name]))

  @spec error(String.t()) :: no_return()
  defp error(msg) do
    IO.puts(:stderr, "error: #{msg}")
    Arc.CLI.Exit.halt(1)
  end
end
