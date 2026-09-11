defmodule Journal.Git do
  @moduledoc "Thin wrapper over the git CLI. All calls run inside the repo."

  def init(repo) do
    unless File.dir?(Path.join(repo, ".git")) do
      File.mkdir_p!(repo)
      {_, 0} = run(repo, ["init", "-q", "-b", "main"])
      {_, 0} = run(repo, ["config", "user.email", "journal@arc"])
      {_, 0} = run(repo, ["config", "user.name", "journal"])
      # Data repo: never run the user's global hooks on journal commits.
      {_, 0} = run(repo, ["config", "core.hooksPath", "/dev/null"])
    end

    :ok
  end

  @doc "Stage the given paths and commit as the caller's public key."
  def commit(repo, paths, from, message) do
    {_, 0} = run(repo, ["add", "--"] ++ paths)
    author = "#{short(from)} <#{from}@arc>"

    case run(repo, ["commit", "-q", "--allow-empty", "--author", author, "-m", message]) do
      {_, 0} -> :ok
      {out, _} -> {:error, out}
    end
  end

  @doc "Short SHA of the last commit touching a path, or nil."
  def rev(repo, path) do
    case run(repo, ["log", "-n1", "--format=%h", "--", path]) do
      {out, 0} -> out |> String.trim() |> blank_to_nil()
      _ -> nil
    end
  end

  def history(repo, path) do
    case run(repo, ["log", "--format=%h %aI %an", "--", path]) do
      {out, 0} -> out |> String.trim()
      _ -> ""
    end
  end

  def push(repo, remote) do
    case run(repo, ["remote", "get-url", "origin"]) do
      {_, 0} -> :ok
      _ -> run(repo, ["remote", "add", "origin", remote])
    end

    run(repo, ["push", "-q", "-u", "origin", "main"])
  end

  def run(repo, args) do
    System.cmd("git", args, cd: repo, stderr_to_stdout: true, env: [{"GIT_TERMINAL_PROMPT", "0"}])
  end

  defp short(pk), do: String.slice(pk, 0, 12)
  defp blank_to_nil(""), do: nil
  defp blank_to_nil(s), do: s
end
