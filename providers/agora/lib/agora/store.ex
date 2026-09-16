defmodule Agora.Store do
  @moduledoc false
  alias Agora.{Config, Post}

  @state "state.json"

  def ensure(root),
    do:
      safe_mkdir(root)
      |> then(fn
        :ok -> safe_mkdir(posts_dir(root))
        error -> error
      end)

  def acquire_lock(root) do
    with :ok <- ensure(root) do
      acquire_lock_dir(lock_dir(root))
    end
  end

  def release_lock(lock) when is_binary(lock) do
    _ = File.rm(Path.join(lock, "pid"))
    _ = File.rmdir(lock)
    :ok
  end

  def put(root, board, post) do
    with {:ok, state} <- load_state(root, board),
         result <- existing_or_new(root, board, state, post) do
      result
    end
  end

  def read(root, board, id) do
    with true <- Post.id?(id),
         {:ok, _state} <- load_state(root, board),
         {:ok, post} <- load_post(root, board, id) do
      {:ok, %{"post" => post}}
    else
      false -> {:error, "invalid_request"}
      {:error, "not_found"} -> {:error, "not_found"}
      _ -> {:error, "corrupt_storage"}
    end
  end

  def feed(root, board, cursor, limit), do: page(root, board, :null, cursor, limit, nil)

  def thread(root, board, id, cursor, limit) do
    with true <- Post.id?(id),
         {:ok, parent} <- read_post(root, board, id),
         {:ok, page} <- page(root, board, id, cursor, limit, parent) do
      {:ok, page}
    else
      false -> {:error, "invalid_request"}
      {:error, "not_found"} -> {:error, "not_found"}
      {:error, _} = error -> error
    end
  end

  defp existing_or_new(root, board, state, post) do
    id = post["id"]

    case Map.fetch(state.by_id, id) do
      {:ok, _} ->
        with {:ok, existing} <- load_post(root, board, id),
             true <- existing == post do
          {:ok, %{"post" => post}}
        else
          false -> {:error, "conflict"}
          _ -> {:error, "corrupt_storage"}
        end

      :error ->
        with :ok <- parent_exists?(state, post),
             {:ok, max_posts} <- Config.max_posts(),
             true <- length(state.records) < max_posts,
             :ok <- ensure_fresh(post["created_at"]),
             :ok <- write_post(root, post),
             :ok <- write_state(root, append_record(state, post)) do
          {:ok, %{"post" => post}}
        else
          false -> {:error, "quota_exceeded"}
          {:error, _} = error -> error
        end
    end
  end

  defp page(root, board, parent, cursor, limit, requested_parent) do
    with true <- is_nil(cursor) or (is_integer(cursor) and cursor > 0),
         true <- is_integer(limit) and limit in 1..50,
         {:ok, state} <- load_state(root, board) do
      matches =
        Enum.filter(state.records, &(&1.parent == parent and (is_nil(cursor) or &1.seq > cursor)))

      chosen = Enum.take(matches, limit)

      with {:ok, posts} <- load_many(root, board, chosen) do
        remaining? = length(matches) > length(chosen)
        next = if remaining?, do: List.last(chosen).seq, else: :null
        reply = %{"posts" => posts, "next" => next}
        {:ok, if(requested_parent, do: Map.put(reply, "post", requested_parent), else: reply)}
      end
    else
      false -> {:error, "invalid_request"}
      {:error, _} = error -> error
    end
  end

  defp read_post(root, board, id) do
    with {:ok, state} <- load_state(root, board),
         true <- Map.has_key?(state.by_id, id),
         {:ok, post} <- load_post(root, board, id) do
      {:ok, post}
    else
      false -> {:error, "not_found"}
      {:error, _} = error -> error
    end
  end

  defp load_many(root, board, records) do
    Enum.reduce_while(records, {:ok, []}, fn record, {:ok, acc} ->
      case load_post(root, board, record.id) do
        {:ok, post} -> {:cont, {:ok, [post | acc]}}
        error -> {:halt, error}
      end
    end)
    |> case do
      {:ok, posts} -> {:ok, Enum.reverse(posts)}
      error -> error
    end
  end

  defp load_state(root, board) do
    with :ok <- ensure(root),
         {:ok, state} <- decode_state(root, board),
         {:ok, ids} <- post_ids(root),
         :ok <- validate_state(state, ids),
         :ok <- validate_posts(root, board, state.records, ids),
         {:ok, records} <- recover_orphans(root, board, state.records, ids) do
      {:ok, %{board: board, records: records, by_id: Map.new(records, &{&1.id, &1})}}
    else
      {:error, _} = error -> error
      _ -> {:error, "corrupt_storage"}
    end
  end

  defp decode_state(root, board) do
    path = state_path(root)

    case File.lstat(path) do
      {:error, :enoent} ->
        {:ok, %{board: board, records: []}}

      {:ok, %{type: :regular}} ->
        with {:ok, data} <- File.read(path),
             %{"version" => 1, "board" => ^board, "records" => records} when is_list(records) <-
               :json.decode(data),
             {:ok, parsed} <- parse_records(records) do
          {:ok, %{board: board, records: parsed}}
        else
          _ -> {:error, "corrupt_storage"}
        end

      _ ->
        {:error, "corrupt_storage"}
    end
  rescue
    _ -> {:error, "corrupt_storage"}
  end

  defp parse_records(records) do
    parsed =
      Enum.map(records, fn
        %{"seq" => seq, "id" => id, "parent" => parent}
        when is_integer(seq) and seq > 0 and is_binary(id) and
               (parent == :null or is_binary(parent)) ->
          %{seq: seq, id: id, parent: parent}

        _ ->
          :invalid
      end)

    if Enum.any?(parsed, &(&1 == :invalid)), do: {:error, "corrupt_storage"}, else: {:ok, parsed}
  end

  defp validate_state(%{records: records}, ids) do
    expected = if records == [], do: [], else: Enum.to_list(1..length(records))
    sequence = Enum.map(records, & &1.seq)
    record_ids = Enum.map(records, & &1.id)
    record_id_set = MapSet.new(record_ids)

    cond do
      sequence != expected ->
        {:error, "corrupt_storage"}

      length(Enum.uniq(record_ids)) != length(record_ids) ->
        {:error, "corrupt_storage"}

      Enum.any?(
        records,
        &(not Post.id?(&1.id) or (&1.parent != :null and not Post.id?(&1.parent)))
      ) ->
        {:error, "corrupt_storage"}

      Enum.any?(
        records,
        &(&1.parent != :null and not MapSet.member?(record_id_set, &1.parent))
      ) ->
        {:error, "corrupt_storage"}

      not parents_precede?(records) ->
        {:error, "corrupt_storage"}

      not MapSet.subset?(record_id_set, MapSet.new(ids)) ->
        {:error, "corrupt_storage"}

      true ->
        :ok
    end
  end

  defp validate_posts(root, board, records, ids) do
    record_by_id = Map.new(records, &{&1.id, &1})

    Enum.reduce_while(ids, :ok, fn id, :ok ->
      case load_post(root, board, id) do
        {:ok, post} ->
          case Map.get(record_by_id, id) do
            nil ->
              {:cont, :ok}

            %{parent: parent} ->
              if parent == post["parent"],
                do: {:cont, :ok},
                else: {:halt, {:error, "corrupt_storage"}}

            _ ->
              {:halt, {:error, "corrupt_storage"}}
          end

        _ ->
          {:halt, {:error, "corrupt_storage"}}
      end
    end)
  end

  defp parents_precede?(records) do
    Enum.reduce_while(records, MapSet.new(), fn record, seen ->
      if record.parent == :null or MapSet.member?(seen, record.parent),
        do: {:cont, MapSet.put(seen, record.id)},
        else: {:halt, :invalid}
    end) != :invalid
  end

  defp recover_orphans(root, board, records, ids) do
    known = MapSet.new(Enum.map(records, & &1.id))
    orphans = ids |> Enum.reject(&MapSet.member?(known, &1)) |> Enum.sort()

    Enum.reduce_while(orphans, {:ok, records}, fn id, {:ok, acc} ->
      case load_post(root, board, id) do
        {:ok, post} ->
          parent = post["parent"]

          if parent == :null or Enum.any?(acc, &(&1.id == parent)) do
            record = %{seq: length(acc) + 1, id: id, parent: parent}
            {:cont, {:ok, acc ++ [record]}}
          else
            {:halt, {:error, "corrupt_storage"}}
          end

        _ ->
          {:halt, {:error, "corrupt_storage"}}
      end
    end)
    |> case do
      {:ok, recovered} when recovered == records ->
        {:ok, records}

      {:ok, recovered} ->
        case write_state(root, %{board: board, records: recovered}) do
          :ok -> {:ok, recovered}
          error -> error
        end

      error ->
        error
    end
  end

  defp append_record(state, post) do
    Map.put(
      state,
      :records,
      state.records ++ [%{seq: length(state.records) + 1, id: post["id"], parent: post["parent"]}]
    )
  end

  defp parent_exists?(state, post) do
    case post["parent"] do
      :null ->
        :ok

      parent when is_binary(parent) ->
        if(Map.has_key?(state.by_id, parent), do: :ok, else: {:error, "not_found"})
    end
  end

  defp ensure_fresh(created_at) do
    if abs(System.system_time(:second) - created_at) <= 300,
      do: :ok,
      else: {:error, "stale_post"}
  end

  defp write_post(root, post) do
    target = post_path(root, post["id"])
    content = post |> :json.encode() |> IO.iodata_to_binary()
    temp = target <> ".tmp-" <> random_suffix()

    case File.open(temp, [:write, :binary, :exclusive]) do
      {:ok, io} ->
        try do
          :ok = IO.binwrite(io, content)
          :ok = :file.sync(io)
          :ok = File.close(io)

          case File.ln(temp, target) do
            :ok ->
              :ok = File.rm(temp)
              :ok

            {:error, :eexist} ->
              _ = File.rm(temp)
              existing_post(target, post)

            _ ->
              _ = File.rm(temp)
              {:error, "storage_failure"}
          end
        after
          File.close(io)
        end

      _ ->
        {:error, "storage_failure"}
    end
  rescue
    _ -> {:error, "storage_failure"}
  end

  defp write_state(root, %{board: board, records: records}) do
    content =
      %{
        "version" => 1,
        "board" => board,
        "records" => Enum.map(records, &%{"seq" => &1.seq, "id" => &1.id, "parent" => &1.parent})
      }
      |> :json.encode()
      |> IO.iodata_to_binary()

    target = state_path(root)
    temp = target <> ".tmp-" <> random_suffix()

    result =
      try do
        case File.open(temp, [:write, :binary, :exclusive]) do
          {:ok, io} ->
            try do
              with :ok <- IO.binwrite(io, content),
                   :ok <- :file.sync(io),
                   :ok <- File.close(io),
                   :ok <- File.rename(temp, target) do
                :ok
              else
                _ -> {:error, "storage_failure"}
              end
            after
              File.close(io)
            end

          _ ->
            {:error, "storage_failure"}
        end
      rescue
        _ -> {:error, "storage_failure"}
      end

    case result do
      :ok ->
        :ok

      error ->
        _ = File.rm(temp)
        error
    end
  end

  defp load_post(root, board, id) do
    path = post_path(root, id)

    with {:ok, %{type: :regular}} <- File.lstat(path),
         {:ok, data} <- File.read(path),
         %{} = post <- :json.decode(data),
         {:ok, valid} <- Post.validate(post, board),
         true <- valid["id"] == id do
      {:ok, valid}
    else
      {:error, :enoent} -> {:error, "not_found"}
      _ -> {:error, "corrupt_storage"}
    end
  rescue
    _ -> {:error, "corrupt_storage"}
  end

  defp post_ids(root) do
    with {:ok, names} <- File.ls(posts_dir(root)) do
      {posts, other} = Enum.split_with(names, &String.ends_with?(&1, ".json"))

      ids = Enum.map(posts, &String.slice(&1, 0, 64))

      if Enum.all?(posts, &(String.length(&1) == 69)) and Enum.all?(ids, &Post.id?/1) and
           Enum.all?(other, &temporary_post?/1),
         do: {:ok, ids},
         else: {:error, "corrupt_storage"}
    else
      _ -> {:error, "corrupt_storage"}
    end
  end

  defp existing_post(target, post) do
    with {:ok, %{type: :regular}} <- File.lstat(target),
         {:ok, data} <- File.read(target),
         ^post <- :json.decode(data) do
      :ok
    else
      _ -> {:error, "conflict"}
    end
  rescue
    _ -> {:error, "conflict"}
  end

  defp temporary_post?(name),
    do: Regex.match?(~r/\A[a-f0-9]{64}\.json\.tmp-[A-Za-z0-9_-]+\z/, name)

  defp acquire_lock_dir(lock) do
    case File.mkdir(lock) do
      :ok ->
        case File.write(Path.join(lock, "pid"), to_string(:os.getpid()), [:binary, :exclusive]) do
          :ok ->
            {:ok, lock}

          _ ->
            _ = File.rmdir(lock)
            {:error, "storage_locked"}
        end

      {:error, :eexist} ->
        recover_stale_lock(lock)

      _ ->
        {:error, "storage_failure"}
    end
  end

  defp recover_stale_lock(lock) do
    pid_path = Path.join(lock, "pid")
    guard = Path.join(Path.dirname(lock), ".lock-reclaim")

    case File.mkdir(guard) do
      :ok ->
        try do
          with {:ok, pid} <- File.read(pid_path),
               pid = String.trim(pid),
               true <- Regex.match?(~r/\A[1-9][0-9]*\z/, pid),
               true <- dead_pid?(pid),
               :ok <- File.rm(pid_path),
               :ok <- File.rmdir(lock) do
            acquire_lock_dir(lock)
          else
            _ -> {:error, "storage_locked"}
          end
        after
          _ = File.rmdir(guard)
        end

      _ ->
        {:error, "storage_locked"}
    end
  end

  defp dead_pid?(pid) do
    case System.cmd("/bin/ps", ["-p", pid, "-o", "pid="], stderr_to_stdout: true) do
      {output, 0} -> String.trim(output) == ""
      {output, 1} -> String.trim(output) == ""
      _ -> false
    end
  rescue
    _ -> false
  end

  defp safe_mkdir(path) do
    case File.mkdir_p(path) do
      :ok ->
        case File.lstat(path) do
          {:ok, %{type: :directory}} -> :ok
          _ -> {:error, "storage_failure"}
        end

      _ ->
        {:error, "storage_failure"}
    end
  end

  defp posts_dir(root), do: Path.join(root, "posts")
  defp post_path(root, id), do: Path.join(posts_dir(root), id <> ".json")
  defp state_path(root), do: Path.join(root, @state)
  defp lock_dir(root), do: Path.join(root, ".lock")
  defp random_suffix, do: :crypto.strong_rand_bytes(12) |> Base.url_encode64(padding: false)
end
