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

  def put_message(root, pk, %{"id" => id} = msg) do
    File.mkdir_p!(msgs_dir(root, pk))
    File.write!(msg_path(root, pk, id), encode(msg))
    :ok
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

  # -- receipts ---------------------------------------------------------------

  def add_receipt(root, pk, id, event) do
    File.mkdir_p!(mailbox(root, pk))
    line = encode(%{"t" => timestamp(), "id" => id, "event" => event})
    File.write!(receipts_path(root, pk), line <> "\n", [:append])
    :ok
  end

  @doc "Receipts for one message id, in file order."
  def receipts(root, pk, id) do
    root |> all_receipts(pk) |> Enum.filter(&(&1["id"] == id))
  end

  @doc "Map of id => MapSet of events."
  def receipt_index(root, pk) do
    root
    |> all_receipts(pk)
    |> Enum.group_by(& &1["id"], & &1["event"])
    |> Map.new(fn {id, events} -> {id, MapSet.new(events)} end)
  end

  defp all_receipts(root, pk) do
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
