defmodule Journal.Command do
  @moduledoc "Dispatches one parsed command line against the store."

  alias Journal.{Store, Page, KPI, Parse, Index, Config}

  @doc """
  Runs one request. The first line of `message` is the command line. Any
  text after the first newline is the request body, which `write` stores as
  the page body when the command line carries no `--body`.
  """
  @spec run(String.t(), String.t(), String.t()) :: {:ok, String.t()} | {:error, String.t()}
  def run(root, from, message) do
    {header, body} = Parse.split(message)
    {args, opts} = Parse.parse(header)

    if from == "" do
      {:error, "forbidden no caller key"}
    else
      dispatch(args, opts, %{root: root, from: from, body: body})
    end
  end

  # -- ls --------------------------------------------------------------------

  defp dispatch(["ls"], _opts, ctx), do: dispatch(["ls", ""], %{}, ctx)

  defp dispatch(["ls", path], _opts, ctx) do
    case String.split(path, "/", trim: true) do
      [] ->
        ctx.root |> Store.list_projects(ctx.from) |> lines_or("no projects")

      [project] ->
        with :ok <- Store.authorize_read(ctx.root, project, ctx.from) do
          ctx.root
          |> Store.list_notebooks(project)
          |> Enum.map(&"#{project}/#{&1}")
          |> lines_or("no notebooks")
        end

      [project, notebook] ->
        with :ok <- Store.authorize_read(ctx.root, project, ctx.from) do
          ctx.root
          |> Store.list_pages(project, notebook)
          |> Enum.map(fn {addr, title} -> "#{addr}\t#{title}" end)
          |> lines_or("no pages")
        end

      _ ->
        {:error, "invalid_address"}
    end
  end

  # -- read / history --------------------------------------------------------

  defp dispatch(["read", addr], opts, ctx) do
    with {:ok, parts} <- Store.address(addr, 3),
         :ok <- Store.authorize_read(ctx.root, hd(parts), ctx.from),
         {:ok, page, rev} <- Store.read_page(ctx.root, parts) do
      text = page |> inject_kpis(ctx.root, parts) |> Page.render() |> slice(opts["lines"])
      {:ok, "rev: #{rev}\n" <> text}
    end
  end

  defp dispatch(["history", addr], _opts, ctx) do
    with {:ok, parts} <- Store.address(addr, 3),
         :ok <- Store.authorize_read(ctx.root, hd(parts), ctx.from) do
      case Store.history(ctx.root, parts) do
        "" -> {:error, "not_found"}
        out -> {:ok, out}
      end
    end
  end

  # -- write / append / edit -------------------------------------------------

  defp dispatch(["write", addr], opts, ctx) do
    with {:ok, parts} <- Store.address(addr, 3),
         {:ok, body} <- write_body(opts["body"], ctx.body),
         :ok <- Store.authorize_write(ctx.root, hd(parts), ctx.from),
         :ok <- Store.check_rev(ctx.root, parts, opts["if_rev"]) do
      existing =
        case Store.read_page(ctx.root, parts) do
          {:ok, page, _} -> page
          _ -> %Page{}
        end

      meta =
        existing.meta
        |> maybe_put("title", opts["title"])
        |> maybe_put("tags", tags(opts["tags"]))

      page = %Page{meta: meta, body: body}

      with {:ok, rev} <- Store.write_page(ctx.root, parts, page, ctx.from, "write #{addr}") do
        {:ok, "rev: #{rev}"}
      end
    end
  end

  defp dispatch(["append", addr | text], _opts, ctx) do
    with {:ok, parts} <- Store.address(addr, 3),
         :ok <- Store.authorize_write(ctx.root, hd(parts), ctx.from) do
      page =
        case Store.read_page(ctx.root, parts) do
          {:ok, page, _} -> page
          _ -> %Page{}
        end

      addition = Enum.join(text, " ")
      body = String.trim_trailing(page.body, "\n") <> "\n\n" <> addition <> "\n"

      with {:ok, rev} <-
             Store.write_page(ctx.root, parts, %{page | body: body}, ctx.from, "append #{addr}") do
        {:ok, "rev: #{rev}"}
      end
    end
  end

  defp dispatch(["edit", addr], opts, ctx) do
    with {:ok, parts} <- Store.address(addr, 3),
         true <- is_binary(opts["if_rev"]) or {:error, "missing --if-rev"},
         true <- is_binary(opts["find"]) or {:error, "missing --find"},
         :ok <- Store.authorize_write(ctx.root, hd(parts), ctx.from),
         :ok <- Store.check_rev(ctx.root, parts, opts["if_rev"]),
         {:ok, page, _} <- Store.read_page(ctx.root, parts) do
      find = opts["find"]
      replace = opts["replace"] || ""

      if String.contains?(page.body, find) do
        body = String.replace(page.body, find, replace, global: false)

        with {:ok, rev} <-
               Store.write_page(ctx.root, parts, %{page | body: body}, ctx.from, "edit #{addr}") do
          {:ok, "rev: #{rev}"}
        end
      else
        {:error, "not_found find string absent"}
      end
    end
  end

  # -- attach / fetch / link -------------------------------------------------

  defp dispatch(["attach", addr], opts, ctx) do
    with {:ok, parts} <- Store.address(addr, 3),
         true <- is_binary(opts["name"]) or {:error, "missing --name"},
         {:ok, bytes} <- decode64(opts["base64"]),
         true <-
           byte_size(bytes) <= Config.max_blob_bytes() or
             {:error, "too_large max #{Config.max_blob_bytes()} bytes"},
         :ok <- Store.authorize_write(ctx.root, hd(parts), ctx.from),
         {:ok, page, _} <- Store.read_page(ctx.root, parts) do
      sha = Store.put_blob(ctx.root, bytes)
      entry = %{"name" => opts["name"], "sha256" => sha, "bytes" => byte_size(bytes)}
      meta = Map.update(page.meta, "attachments", [entry], &(&1 ++ [entry]))

      with {:ok, rev} <-
             Store.write_page(
               ctx.root,
               parts,
               %{page | meta: meta},
               ctx.from,
               "attach #{addr} #{opts["name"]}"
             ) do
        {:ok, "rev: #{rev}\nsha256: #{sha}"}
      end
    end
  end

  defp dispatch(["fetch", sha], _opts, ctx) do
    case Store.get_blob(ctx.root, sha) do
      {:ok, bytes} -> {:ok, Base.encode64(bytes)}
      _ -> {:error, "not_found"}
    end
  end

  defp dispatch(["link", addr, uri], opts, ctx) do
    with {:ok, parts} <- Store.address(addr, 3),
         true <- valid_uri?(uri) or {:error, "invalid_uri"},
         :ok <- Store.authorize_write(ctx.root, hd(parts), ctx.from),
         {:ok, page, _} <- Store.read_page(ctx.root, parts) do
      entry =
        %{"uri" => uri}
        |> maybe_put("name", opts["name"])
        |> maybe_put("sha256", opts["sha256"])
        |> maybe_put("host", if(String.starts_with?(uri, "file://"), do: ctx.from))

      meta = Map.update(page.meta, "links", [entry], &(&1 ++ [entry]))

      with {:ok, rev} <-
             Store.write_page(ctx.root, parts, %{page | meta: meta}, ctx.from, "link #{addr}") do
        {:ok, "rev: #{rev}"}
      end
    end
  end

  # -- kpi -------------------------------------------------------------------

  defp dispatch(["kpi", "set", nb, key, value], opts, ctx) do
    with {:ok, parts} <- Store.address(nb, 2),
         {:ok, number} <- parse_number(value),
         :ok <- Store.authorize_write(ctx.root, hd(parts), ctx.from) do
      record =
        %{"t" => Store.timestamp(), "by" => ctx.from, "key" => key, "value" => number}
        |> maybe_put("ref", opts["ref"])
        |> maybe_put("note", opts["note"])

      with :ok <- Store.kpi_append(ctx.root, parts, record, ctx.from) do
        {:ok, KPI.format(record)}
      end
    end
  end

  defp dispatch(["kpi", "log", nb, key], _opts, ctx) do
    with {:ok, parts} <- Store.address(nb, 2),
         :ok <- Store.authorize_read(ctx.root, hd(parts), ctx.from) do
      ctx.root
      |> Store.kpi_file(parts)
      |> KPI.log(key)
      |> Enum.map(&KPI.format/1)
      |> lines_or("no records")
    end
  end

  defp dispatch(["kpi", "latest", nb], _opts, ctx) do
    with {:ok, parts} <- Store.address(nb, 2),
         :ok <- Store.authorize_read(ctx.root, hd(parts), ctx.from) do
      ctx.root
      |> Store.kpi_file(parts)
      |> KPI.latest()
      |> Enum.map(&KPI.format/1)
      |> lines_or("no records")
    end
  end

  # -- search ----------------------------------------------------------------

  defp dispatch(["search" | words], opts, ctx) do
    query = Enum.join(words, " ")
    scope = opts["notebook"] || opts["project"]

    with true <- query != "" or {:error, "missing query"},
         {:ok, out} <- Index.search(ctx.root, query, opts["deep"] == true) do
      out
      |> String.split("\n")
      |> Enum.map(&String.replace(&1, ~r{^\./?(?:.*?/)?projects/}, ""))
      |> Enum.filter(fn line ->
        scope == nil or not String.contains?(line, "/") or String.starts_with?(line, scope)
      end)
      |> Enum.filter(fn line ->
        scope == nil or not Regex.match?(~r{^[a-z0-9]}, line) or String.starts_with?(line, scope)
      end)
      |> Enum.filter(&(&1 != ""))
      |> Enum.filter(&(scope == nil or allowed_line?(ctx, &1)))
      |> lines_or("no results")
    end
  end

  # -- acl -------------------------------------------------------------------

  defp dispatch(["acl", project, "ls"], _opts, ctx) do
    with :ok <- Store.authorize_read(ctx.root, project, ctx.from) do
      ctx.root |> Store.acl(project) |> lines_or("no acl")
    end
  end

  defp dispatch(["acl", project, op, pubkey], _opts, ctx) when op in ["add", "rm"] do
    with :ok <- authorize_owner(ctx, project),
         true <- Regex.match?(~r/^[a-f0-9]{16,}$/, pubkey) or {:error, "invalid_pubkey"} do
      keys = Store.acl(ctx.root, project)

      keys =
        case op do
          "add" -> Enum.uniq(keys ++ [pubkey])
          "rm" -> if pubkey == hd(keys), do: keys, else: keys -- [pubkey]
        end

      with :ok <-
             Store.write_acl(
               ctx.root,
               project,
               keys,
               ctx.from,
               "acl #{project} #{op} #{String.slice(pubkey, 0, 12)}"
             ) do
        {:ok, Enum.join(keys, "\n")}
      end
    end
  end

  defp dispatch([], _opts, _ctx), do: {:ok, help()}
  defp dispatch(["help"], _opts, _ctx), do: {:ok, help()}
  defp dispatch([cmd | _], _opts, _ctx), do: {:error, "unknown_command #{cmd}"}

  # -- helpers ---------------------------------------------------------------

  defp help do
    """
    journal commands
      ls [project[/notebook]]
      read <p/n/page> [--lines a:b]
      write <p/n/page> [--title t] [--tags a,b] [--if-rev r] [--body "..."]
        the text after the first line is the page body when --body is absent
      append <p/n/page> <text>
      edit <p/n/page> --if-rev r --find s --replace t
      attach <p/n/page> --name f --base64 b
      fetch <sha256>
      link <p/n/page> <uri> [--name n] [--sha256 h]
      kpi set <p/n> <key> <value> [--ref r] [--note n]
      kpi log <p/n> <key>
      kpi latest <p/n>
      search <query> [--project p] [--notebook p/n] [--deep]
      history <p/n/page>
      acl <project> add|rm|ls [pubkey]
    """
    |> String.trim_trailing()
  end

  defp authorize_owner(ctx, project) do
    cond do
      not Store.project_exists?(ctx.root, project) -> {:error, "not_found"}
      Store.owner(ctx.root, project) == ctx.from -> :ok
      true -> {:error, "forbidden owner only"}
    end
  end

  defp allowed_line?(ctx, line) do
    case String.split(line, "/", parts: 2) do
      [project, _] -> Store.allowed?(ctx.root, project, ctx.from)
      _ -> true
    end
  end

  defp inject_kpis(%Page{} = page, root, [project, notebook, _]) do
    if String.contains?(page.body, "<!-- kpi:") do
      latest =
        root |> Store.kpi_file([project, notebook]) |> KPI.latest() |> Map.new(&{&1["key"], &1})

      body =
        Regex.replace(~r/<!-- kpi: *([^ >]+) *-->/, page.body, fn whole, key ->
          case latest[key] do
            nil -> whole
            r -> "#{key} = #{r["value"]} (#{r["t"]})"
          end
        end)

      %{page | body: body}
    else
      page
    end
  end

  defp slice(text, nil), do: text

  defp slice(text, range) do
    case Regex.run(~r/^(\d+):(\d+)$/, range) do
      [_, a, b] ->
        {a, b} = {String.to_integer(a), String.to_integer(b)}
        text |> String.split("\n") |> Enum.slice((a - 1)..(b - 1)//1) |> Enum.join("\n")

      _ ->
        text
    end
  end

  # The page body comes from `--body` on the command line or from the request
  # body, the text after the first line of the message. Both are stored as
  # given. Setting both is an error.
  defp write_body(nil, nil),
    do: {:error, "missing body: pass --body or send it after the first line"}

  defp write_body(nil, body), do: {:ok, trailing_newline(body)}
  defp write_body(opt, body) when not is_binary(opt), do: write_body(nil, body)
  defp write_body(opt, nil), do: {:ok, trailing_newline(opt)}
  defp write_body(opt, ""), do: write_body(opt, nil)
  defp write_body(_opt, _body), do: {:error, "invalid_arguments --body and request body both set"}

  defp trailing_newline(""), do: ""
  defp trailing_newline(body), do: if(String.ends_with?(body, "\n"), do: body, else: body <> "\n")

  defp tags(nil), do: nil
  defp tags(s), do: s |> String.split(",", trim: true) |> Enum.map(&String.trim/1)

  defp maybe_put(map, _k, nil), do: map
  defp maybe_put(map, k, v), do: Map.put(map, k, v)

  defp decode64(nil), do: {:error, "missing --base64"}

  defp decode64(s) do
    case Base.decode64(s, ignore: :whitespace) do
      {:ok, bytes} -> {:ok, bytes}
      :error -> {:error, "invalid_base64"}
    end
  end

  defp valid_uri?(uri), do: Regex.match?(~r/^[a-z][a-z0-9+.-]*:.+/i, uri)

  defp parse_number(s) do
    case Float.parse(s) do
      {f, ""} -> {:ok, if(Regex.match?(~r/^-?\d+$/, s), do: String.to_integer(s), else: f)}
      _ -> {:error, "invalid_number"}
    end
  end

  defp lines_or([], empty), do: {:ok, empty}
  defp lines_or(lines, _empty), do: {:ok, Enum.join(lines, "\n")}
end
