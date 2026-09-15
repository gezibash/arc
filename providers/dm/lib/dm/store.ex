defmodule Dm.Store do
  @moduledoc """
  Filesystem layout under DM_ROOT.

    mailboxes/<pubkey>/msgs/<id>.json
    mailboxes/<pubkey>/receipts.jsonl
    mailboxes/<pubkey>/blocked

  Every body on disk is a sealed-v1 token. The store never sees plaintext.
  """

  @pubkey ~r/^[a-f0-9]{64}$/

  def ensure(root) do
    File.mkdir_p!(Path.join(root, "mailboxes"))
    :ok
  end

  def pubkey?(s), do: is_binary(s) and Regex.match?(@pubkey, s)

  def mailbox(root, pk), do: Path.join([root, "mailboxes", pk])
  defp msgs_dir(root, pk), do: Path.join(mailbox(root, pk), "msgs")
  defp msg_path(root, pk, id), do: Path.join(msgs_dir(root, pk), id <> ".json")
  defp receipts_path(root, pk), do: Path.join(mailbox(root, pk), "receipts.jsonl")
  defp blocked_path(root, pk), do: Path.join(mailbox(root, pk), "blocked")

  def timestamp, do: DateTime.utc_now() |> DateTime.truncate(:second) |> DateTime.to_iso8601()

  # -- messages ---------------------------------------------------------------

  @doc """
  Store one message. Returns the bytes written. The mailbox counter moves
  by the change in file size unless `counted: false`, in which case the
  caller settles the counter with `bump_usage/3`.
  """
  def put_message(root, pk, %{"id" => id} = msg, opts \\ []) do
    File.mkdir_p!(msgs_dir(root, pk))
    write(root, pk, msg_path(root, pk, id), encode(msg), opts)
  end

  def get_message(root, pk, id) do
    case File.read(msg_path(root, pk, id)) do
      {:ok, json} -> {:ok, :json.decode(json)}
      _ -> {:error, "not_found"}
    end
  end

  @doc "All messages in a mailbox, oldest first. Ids sort by time."
  def list_messages(root, pk) do
    case File.ls(msgs_dir(root, pk)) do
      {:ok, files} ->
        files
        |> Enum.filter(&String.ends_with?(&1, ".json"))
        |> Enum.sort()
        |> Enum.map(&(msgs_dir(root, pk) |> Path.join(&1) |> File.read!() |> :json.decode()))

      _ ->
        []
    end
  end

  @doc "Remove one message and its blobs and settle the counter. Missing files are fine."
  def delete_message(root, pk, id) do
    freed = remove_message(root, pk, id)
    bump_usage(root, pk, -freed)
  end

  @doc """
  Remove one message and its blobs without touching the counter. Returns
  the bytes that actually left the disk, so a refused unlink counts as zero.
  """
  def remove_message(root, pk, id) do
    paths = [msg_path(root, pk, id), Path.join(blobs_dir(root, pk, id), "*")]
    before = Enum.sum(Enum.map(paths, &tree_size/1))
    File.rm(msg_path(root, pk, id))
    File.rm_rf(blobs_dir(root, pk, id))
    before - Enum.sum(Enum.map(paths, &tree_size/1))
  end

  # -- attachments ------------------------------------------------------------

  def put_blob(root, pk, id, name, token, opts \\ []) do
    File.mkdir_p!(blobs_dir(root, pk, id))
    write(root, pk, Path.join(blobs_dir(root, pk, id), name), token, opts)
  end

  def get_blob(root, pk, id, name) do
    case File.read(Path.join(blobs_dir(root, pk, id), name)) do
      {:ok, token} -> {:ok, token}
      _ -> {:error, "not_found"}
    end
  end

  # -- budget -----------------------------------------------------------------

  @doc """
  Bytes used by a mailbox: message files and blobs. The count lives in the
  `usage` file and every write adjusts it. A missing, unreadable, or
  malformed counter is rebuilt from a walk of the mailbox.
  """
  def usage(root, pk) do
    with {:ok, text} <- File.read(usage_path(root, pk)),
         {used, ""} when used >= 0 <- Integer.parse(String.trim(text)) do
      used
    else
      _ -> recount_usage(root, pk)
    end
  end

  @doc "Walk a mailbox, store its byte count, and return it. Repairs a drifted counter."
  def recount_usage(root, pk) do
    File.mkdir_p!(mailbox(root, pk))

    used =
      tree_size(Path.join(msgs_dir(root, pk), "*.json")) +
        tree_size(Path.join(blobs_dir(root, pk, "*"), "*"))

    write_usage(root, pk, used)
    used
  end

  @doc "Move the mailbox counter by `delta` bytes. A result below zero triggers a recount."
  def bump_usage(_root, _pk, 0), do: :ok

  def bump_usage(root, pk, delta) do
    case usage(root, pk) + delta do
      used when used >= 0 ->
        write_usage(root, pk, used)

      _ ->
        recount_usage(root, pk)
        :ok
    end
  end

  @doc "Delete messages and blobs with an id below `before` from one mailbox. Returns the count."
  def purge(root, pk, before) do
    ids =
      root
      |> list_messages(pk)
      |> Enum.map(& &1["id"])
      |> Enum.filter(&(&1 < before))

    freed = ids |> Enum.map(&remove_message(root, pk, &1)) |> Enum.sum()
    bump_usage(root, pk, -freed)
    length(ids)
  end

  # Writes a file. With `counted: true` (the default) the counter moves by
  # the change in size; it is read before the write so a first walk
  # excludes the new file.
  defp write(root, pk, path, data, opts) do
    if Keyword.get(opts, :counted, true) do
      base = usage(root, pk)
      before = file_size(path)
      File.write!(path, data)
      write_usage(root, pk, base + byte_size(data) - before)
    else
      File.write!(path, data)
    end

    {:ok, byte_size(data)}
  end

  # Written to a sibling and renamed, so a reader never sees a torn file.
  defp write_usage(root, pk, used) do
    path = usage_path(root, pk)
    File.write!(path <> ".tmp", Integer.to_string(used))
    File.rename!(path <> ".tmp", path)
    :ok
  end

  defp usage_path(root, pk), do: Path.join(mailbox(root, pk), "usage")
  defp blobs_dir(root, pk, id), do: Path.join([mailbox(root, pk), "blobs", id])

  defp file_size(path) do
    case File.stat(path) do
      {:ok, %{size: size}} -> size
      _ -> 0
    end
  end

  defp tree_size(glob),
    do: glob |> Path.wildcard(match_dot: true) |> Enum.map(&file_size/1) |> Enum.sum()

  # -- receipts ---------------------------------------------------------------

  def add_receipt(root, pk, id, event, extra \\ %{}) do
    File.mkdir_p!(mailbox(root, pk))
    line = encode(Map.merge(extra, %{"t" => timestamp(), "id" => id, "event" => event}))
    File.write!(receipts_path(root, pk), line <> "\n", [:append])
    :ok
  end

  @doc """
  Current reactions per message id: `%{id => [{by, value}]}`. The latest
  reaction by one key wins, and an empty value clears it.
  """
  def reaction_index(receipts) when is_list(receipts) do
    receipts
    |> Enum.filter(&(&1["event"] == "reaction"))
    |> Enum.group_by(& &1["id"])
    |> Map.new(fn {id, rs} ->
      current =
        rs
        |> Enum.reduce(%{}, fn r, acc -> Map.put(acc, r["by"], r["value"]) end)
        |> Enum.reject(fn {_by, v} -> v == "" end)
        |> Enum.sort()

      {id, current}
    end)
  end

  @doc "Receipts for one message id, in file order."
  def receipts(root, pk, id) do
    root |> all_receipts(pk) |> Enum.filter(&(&1["id"] == id))
  end

  @doc "Map of id => MapSet of events."
  def receipt_index(root, pk), do: root |> all_receipts(pk) |> receipt_index()

  def receipt_index(receipts) when is_list(receipts) do
    receipts
    |> Enum.group_by(& &1["id"], & &1["event"])
    |> Map.new(fn {id, events} -> {id, MapSet.new(events)} end)
  end

  @doc "Every receipt in a mailbox, in file order. Load once per command."
  def all_receipts(root, pk) do
    case File.read(receipts_path(root, pk)) do
      {:ok, text} ->
        text |> String.split("\n", trim: true) |> Enum.map(&:json.decode/1)

      _ ->
        []
    end
  end

  # -- block list -------------------------------------------------------------

  def blocked(root, pk), do: read_lines(blocked_path(root, pk))

  def write_blocked(root, pk, keys) do
    File.mkdir_p!(mailbox(root, pk))
    File.write!(blocked_path(root, pk), Enum.map_join(keys, "", &(&1 <> "\n")))
    :ok
  end

  def blocked?(root, pk, sender), do: sender in blocked(root, pk)

  # -- mute list --------------------------------------------------------------

  def muted(root, pk), do: read_lines(Path.join(mailbox(root, pk), "muted"))

  def write_muted(root, pk, keys) do
    File.mkdir_p!(mailbox(root, pk))
    File.write!(Path.join(mailbox(root, pk), "muted"), Enum.map_join(keys, "", &(&1 <> "\n")))
    :ok
  end

  # -- settings ---------------------------------------------------------------

  @default_settings %{"receipts" => "on"}

  def settings(root, pk) do
    case File.read(Path.join(mailbox(root, pk), "settings.json")) do
      {:ok, json} -> Map.merge(@default_settings, :json.decode(json))
      _ -> @default_settings
    end
  end

  def write_settings(root, pk, settings) do
    File.mkdir_p!(mailbox(root, pk))
    File.write!(Path.join(mailbox(root, pk), "settings.json"), encode(settings))
    :ok
  end

  defp read_lines(path) do
    case File.read(path) do
      {:ok, text} -> String.split(text, "\n", trim: true)
      _ -> []
    end
  end

  defp encode(map), do: map |> :json.encode() |> IO.iodata_to_binary()
end
