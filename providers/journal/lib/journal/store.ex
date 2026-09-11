defmodule Journal.Store do
  @moduledoc """
  Filesystem layout under JOURNAL_ROOT and the operations on it.

    repo/projects/<project>/ACL
    repo/projects/<project>/<notebook>/<page>.md
    repo/projects/<project>/<notebook>/kpi.jsonl
    blobs/<sha256>
  """

  alias Journal.{Git, Page, KPI}

  @segment ~r/^[a-z0-9][a-z0-9._-]*$/

  def ensure(root) do
    File.mkdir_p!(Path.join(root, "blobs"))
    File.mkdir_p!(Path.join(root, "repo/projects"))
    Git.init(repo(root))
  end

  def repo(root), do: Path.join(root, "repo")
  def projects_dir(root), do: Path.join(root, "repo/projects")

  # -- addresses -------------------------------------------------------------

  @doc "Split and validate an address with exactly `n` segments."
  def address(addr, n) do
    parts = String.split(addr || "", "/", trim: true)

    if length(parts) == n and Enum.all?(parts, &Regex.match?(@segment, &1)) do
      {:ok, parts}
    else
      {:error, "invalid_address"}
    end
  end

  def page_path([project, notebook, page]),
    do: Path.join(["projects", project, notebook, page <> ".md"])

  def kpi_path([project, notebook]), do: Path.join(["projects", project, notebook, "kpi.jsonl"])
  def acl_path(project), do: Path.join(["projects", project, "ACL"])

  # -- acl -------------------------------------------------------------------

  def acl(root, project) do
    case File.read(Path.join(repo(root), acl_path(project))) do
      {:ok, body} -> String.split(body, "\n", trim: true)
      _ -> []
    end
  end

  def owner(root, project), do: List.first(acl(root, project))

  @doc "True if `from` may access the project. A project without an ACL is open to its creator only."
  def allowed?(root, project, from), do: from in acl(root, project)

  def project_exists?(root, project), do: File.dir?(Path.join(projects_dir(root), project))

  @doc "Create the project with `from` as owner if it does not exist. Returns :ok or {:error, :forbidden}."
  def authorize_write(root, project, from) do
    cond do
      not project_exists?(root, project) ->
        write_acl(root, project, [from], from, "create project #{project}")
        :ok

      allowed?(root, project, from) ->
        :ok

      true ->
        {:error, "forbidden"}
    end
  end

  def authorize_read(root, project, from) do
    if allowed?(root, project, from), do: :ok, else: {:error, "forbidden"}
  end

  def write_acl(root, project, keys, from, message) do
    path = acl_path(project)
    abs = Path.join(repo(root), path)
    File.mkdir_p!(Path.dirname(abs))
    File.write!(abs, Enum.join(keys, "\n") <> "\n")
    commit(root, [path], from, message)
  end

  # -- pages -----------------------------------------------------------------

  def read_page(root, parts) do
    path = page_path(parts)

    case File.read(Path.join(repo(root), path)) do
      {:ok, text} -> {:ok, Page.parse(text), Git.rev(repo(root), path)}
      _ -> {:error, "not_found"}
    end
  end

  def rev(root, parts), do: Git.rev(repo(root), page_path(parts))

  @doc "Write a page and commit. Sets author on create and updated on every write."
  def write_page(root, parts, %Page{} = page, from, message) do
    path = page_path(parts)
    abs = Path.join(repo(root), path)
    now = timestamp()

    meta =
      page.meta
      |> Map.put_new("created", now)
      |> Map.put_new("author", from)
      |> Map.put("updated", now)

    File.mkdir_p!(Path.dirname(abs))
    File.write!(abs, Page.render(%{page | meta: meta}))

    with :ok <- commit(root, [path], from, message) do
      {:ok, Git.rev(repo(root), path)}
    end
  end

  def check_rev(_root, _parts, nil), do: :ok

  def check_rev(root, parts, expected) do
    case rev(root, parts) do
      ^expected -> :ok
      current -> {:error, "conflict current rev: #{current || "none"}"}
    end
  end

  def history(root, parts), do: Git.history(repo(root), page_path(parts))

  # -- listing ---------------------------------------------------------------

  def list_projects(root, from) do
    projects_dir(root)
    |> ls_dirs()
    |> Enum.filter(&allowed?(root, &1, from))
  end

  def list_notebooks(root, project), do: ls_dirs(Path.join(projects_dir(root), project))

  def list_pages(root, project, notebook) do
    dir = Path.join([projects_dir(root), project, notebook])

    dir
    |> File.ls()
    |> case do
      {:ok, files} -> files
      _ -> []
    end
    |> Enum.filter(&String.ends_with?(&1, ".md"))
    |> Enum.sort()
    |> Enum.map(fn file ->
      page = Path.rootname(file)

      title =
        dir
        |> Path.join(file)
        |> File.read!()
        |> Page.parse()
        |> Map.get(:meta)
        |> Map.get("title", "")

      {"#{project}/#{notebook}/#{page}", title}
    end)
  end

  defp ls_dirs(dir) do
    case File.ls(dir) do
      {:ok, entries} -> entries |> Enum.filter(&File.dir?(Path.join(dir, &1))) |> Enum.sort()
      _ -> []
    end
  end

  # -- kpi -------------------------------------------------------------------

  def kpi_append(root, parts, record, from) do
    path = kpi_path(parts)
    KPI.append(Path.join(repo(root), path), record)
    commit(root, [path], from, "kpi #{Enum.join(parts, "/")} #{record["key"]}=#{record["value"]}")
  end

  def kpi_file(root, parts), do: Path.join(repo(root), kpi_path(parts))

  # -- blobs -----------------------------------------------------------------

  def put_blob(root, bytes) do
    sha = :crypto.hash(:sha256, bytes) |> Base.encode16(case: :lower)
    path = Path.join([root, "blobs", sha])
    unless File.exists?(path), do: File.write!(path, bytes)
    sha
  end

  def get_blob(root, sha) do
    if Regex.match?(~r/^[a-f0-9]{64}$/, sha) do
      File.read(Path.join([root, "blobs", sha]))
    else
      {:error, :enoent}
    end
  end

  # -- helpers ---------------------------------------------------------------

  defp commit(root, paths, from, message) do
    with :ok <- Git.commit(repo(root), paths, from, message) do
      Journal.Index.touch()
      Journal.Push.touch()
      :ok
    end
  end

  def timestamp, do: DateTime.utc_now() |> DateTime.truncate(:second) |> DateTime.to_iso8601()
end
