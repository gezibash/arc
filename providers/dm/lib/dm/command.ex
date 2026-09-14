defmodule Dm.Command do
  @moduledoc "Dispatches one parsed command line against the store."

  alias Dm.{Config, Parse, Store, ULID}

  @token_re ~r/^sealed-v1:[A-Za-z0-9+\/=]+$/

  @spec run(String.t(), String.t(), String.t()) :: {:ok, String.t()} | {:error, String.t()}
  def run(root, from, message) do
    {header, body} = Parse.split(message)
    {args, opts} = Parse.parse(header)

    if Store.pubkey?(from) do
      dispatch(args, opts, %{root: root, from: from, body: body})
    else
      {:error, "forbidden no caller key"}
    end
  end

  # -- send -------------------------------------------------------------------

  defp dispatch(["send", peer], opts, ctx) do
    with :ok <- valid_peer(peer),
         {:ok, to_peer, to_self} <- sealed_pair(ctx.body),
         false <- Store.blocked?(ctx.root, peer, ctx.from) && {:error, "blocked"} do
      id = ULID.generate()

      base =
        %{
          "id" => id,
          "from" => ctx.from,
          "to" => peer,
          "t" => Store.timestamp(),
          "enc" => "sealed-v1"
        }
        |> maybe_put("reply_to", opts["reply_to"])

      :ok = Store.put_message(ctx.root, peer, Map.put(base, "body", to_peer))
      :ok = Store.add_receipt(ctx.root, peer, id, "delivered")

      if peer != ctx.from do
        :ok = Store.put_message(ctx.root, ctx.from, Map.put(base, "body", to_self))
      end

      {:ok, "id: #{id}"}
    end
  end

  # -- conversations ----------------------------------------------------------

  defp dispatch(["conversations"], opts, ctx) do
    index = Store.receipt_index(ctx.root, ctx.from)
    muted = Store.muted(ctx.root, ctx.from)

    conversations =
      ctx.root
      |> Store.list_messages(ctx.from)
      |> Enum.group_by(&other(&1, ctx))
      |> Enum.map(fn {peer, msgs} ->
        last = List.last(msgs)
        is_muted = peer in muted

        unread =
          if is_muted,
            do: 0,
            else: Enum.count(msgs, &(not outbound?(&1, ctx) and not read?(&1, index)))

        {peer, last, unread, is_muted}
      end)
      |> Enum.sort_by(fn {_, last, _, _} -> last["id"] end, :desc)
      |> limit_first(opts["limit"])

    total_unread = conversations |> Enum.map(&elem(&1, 2)) |> Enum.sum()

    header = "#{total_unread} unread in #{length(conversations)} conversations"

    rows =
      Enum.map(conversations, fn {peer, last, unread, is_muted} ->
        muted_flag = if is_muted, do: "muted", else: "-"
        "#{peer}\t#{last["id"]}\t#{last["t"]}\t#{unread}\t#{muted_flag}\t#{last["body"]}"
      end)

    {:ok, Enum.join([header | rows], "\n")}
  end

  # -- inbox / thread ---------------------------------------------------------

  defp dispatch(["inbox"], opts, ctx) do
    index = Store.receipt_index(ctx.root, ctx.from)
    muted = Store.muted(ctx.root, ctx.from)

    ctx.root
    |> Store.list_messages(ctx.from)
    |> Enum.reject(&archived?(&1, index))
    |> Enum.reject(
      &(opts["unread"] == true and
          (outbound?(&1, ctx) or read?(&1, index) or other(&1, ctx) in muted))
    )
    |> since(opts["since"])
    |> limit(opts["limit"])
    |> Enum.map(&line(&1, ctx))
    |> lines_or("no messages")
  end

  defp dispatch(["thread", peer], opts, ctx) do
    with :ok <- valid_peer(peer) do
      msgs =
        ctx.root
        |> Store.list_messages(ctx.from)
        |> Enum.filter(&(other(&1, ctx) == peer))
        |> since(opts["since"])
        |> limit(opts["limit"])

      if opts["bodies"] == true do
        thread_with_bodies(msgs, peer, ctx)
      else
        msgs |> Enum.map(&line(&1, ctx)) |> lines_or("no messages")
      end
    end
  end

  # Returns every message with its body and marks the inbound ones read.
  # Line: id, dir, peer, t, reply_to or -, flags, body. This format is part
  # of the CLI interface; the conversation renderer reads it.
  defp thread_with_bodies(msgs, peer, ctx) do
    index = Store.receipt_index(ctx.root, ctx.from)
    reactions = Store.reaction_index(ctx.root, ctx.from)
    show_read = Store.settings(ctx.root, peer)["receipts"] == "on"
    peer_index = if show_read, do: Store.receipt_index(ctx.root, peer), else: %{}
    unread = Enum.filter(msgs, &(not outbound?(&1, ctx) and not read?(&1, index)))

    rows =
      Enum.map(msgs, fn msg ->
        state =
          cond do
            retracted?(msg, index) -> "retracted"
            outbound?(msg, ctx) and read?(msg, peer_index) -> "read"
            outbound?(msg, ctx) -> "delivered"
            read?(msg, index) -> "read"
            true -> "unread"
          end

        flags = flags_field(state, Map.get(reactions, msg["id"], []))
        dir = if outbound?(msg, ctx), do: "out", else: "in"

        Enum.join(
          [msg["id"], dir, other(msg, ctx), msg["t"], msg["reply_to"] || "-", flags, msg["body"]],
          "\t"
        )
      end)

    Enum.each(unread, &Store.add_receipt(ctx.root, ctx.from, &1["id"], "read"))

    header = "#{peer} · #{length(msgs)} messages, #{length(unread)} unread"
    {:ok, Enum.join([header | rows], "\n")}
  end

  # -- read / ack / status / archive -----------------------------------------

  defp dispatch(["read", id], _opts, ctx) do
    with :ok <- valid_id(id),
         {:ok, msg} <- Store.get_message(ctx.root, ctx.from, id) do
      if not outbound?(msg, ctx) and not read?(msg, Store.receipt_index(ctx.root, ctx.from)) do
        :ok = Store.add_receipt(ctx.root, ctx.from, id, "read")
      end

      index = Store.receipt_index(ctx.root, ctx.from)
      reactions = ctx.root |> Store.reaction_index(ctx.from) |> Map.get(id, [])
      {:ok, render(msg, retracted?(msg, index), reactions)}
    end
  end

  defp dispatch(["ack" | ids], _opts, ctx) when ids != [] do
    with :ok <- receipt_each(ids, ctx, "read", &(not outbound?(&1, ctx))) do
      {:ok, "acked #{length(ids)}"}
    end
  end

  defp dispatch(["archive" | ids], _opts, ctx) when ids != [] do
    with :ok <- receipt_each(ids, ctx, "archived", fn _ -> true end) do
      {:ok, "archived #{length(ids)}"}
    end
  end

  defp dispatch(["status", id], _opts, ctx) do
    with :ok <- valid_id(id),
         {:ok, msg} <- Store.get_message(ctx.root, ctx.from, id),
         true <- outbound?(msg, ctx) or {:error, "forbidden not the sender"} do
      # The recipient decides whether senders see read receipts.
      show_read = Store.settings(ctx.root, msg["to"])["receipts"] == "on"

      ctx.root
      |> Store.receipts(msg["to"], id)
      |> Enum.filter(&(&1["event"] == "delivered" or (show_read and &1["event"] == "read")))
      |> Enum.map(&"#{&1["event"]} #{&1["t"]}")
      |> lines_or("no receipts")
    end
  end

  # -- react / retract --------------------------------------------------------

  defp dispatch(["react", id, value], _opts, ctx) do
    with :ok <- valid_id(id),
         :ok <- valid_reaction(value),
         {:ok, msg} <- Store.get_message(ctx.root, ctx.from, id) do
      value = if value == "none", do: "", else: value
      extra = %{"by" => ctx.from, "value" => value}

      msg
      |> holders()
      |> Enum.each(&Store.add_receipt(ctx.root, &1, id, "reaction", extra))

      {:ok, if(value == "", do: "cleared #{id}", else: "reacted #{value} #{id}")}
    end
  end

  defp dispatch(["retract", id], _opts, ctx) do
    with :ok <- valid_id(id),
         {:ok, msg} <- Store.get_message(ctx.root, ctx.from, id),
         true <- outbound?(msg, ctx) or {:error, "forbidden not the sender"},
         true <- within_retract_window?(msg) or {:error, "too_late"} do
      msg
      |> holders()
      |> Enum.reject(&(&1 == ctx.from))
      |> Enum.each(fn pk ->
        {:ok, copy} = Store.get_message(ctx.root, pk, id)
        :ok = Store.put_message(ctx.root, pk, Map.put(copy, "body", ""))
      end)

      msg |> holders() |> Enum.each(&Store.add_receipt(ctx.root, &1, id, "retracted"))
      {:ok, "retracted #{id}"}
    end
  end

  # -- block list -------------------------------------------------------------

  defp dispatch(["block", peer], _opts, ctx) do
    with :ok <- valid_peer(peer) do
      keys = Enum.uniq(Store.blocked(ctx.root, ctx.from) ++ [peer])
      :ok = Store.write_blocked(ctx.root, ctx.from, keys)
      {:ok, "blocked #{peer}"}
    end
  end

  defp dispatch(["unblock", peer], _opts, ctx) do
    with :ok <- valid_peer(peer) do
      keys = Store.blocked(ctx.root, ctx.from) -- [peer]
      :ok = Store.write_blocked(ctx.root, ctx.from, keys)
      {:ok, "unblocked #{peer}"}
    end
  end

  defp dispatch(["blocked"], _opts, ctx) do
    ctx.root |> Store.blocked(ctx.from) |> lines_or("no blocked keys")
  end

  # -- mute list --------------------------------------------------------------

  defp dispatch(["mute", peer], _opts, ctx) do
    with :ok <- valid_peer(peer) do
      keys = Enum.uniq(Store.muted(ctx.root, ctx.from) ++ [peer])
      :ok = Store.write_muted(ctx.root, ctx.from, keys)
      {:ok, "muted #{peer}"}
    end
  end

  defp dispatch(["unmute", peer], _opts, ctx) do
    with :ok <- valid_peer(peer) do
      keys = Store.muted(ctx.root, ctx.from) -- [peer]
      :ok = Store.write_muted(ctx.root, ctx.from, keys)
      {:ok, "unmuted #{peer}"}
    end
  end

  defp dispatch(["muted"], _opts, ctx) do
    ctx.root |> Store.muted(ctx.from) |> lines_or("no muted keys")
  end

  # -- settings ---------------------------------------------------------------

  defp dispatch(["settings"], _opts, ctx) do
    ctx.root
    |> Store.settings(ctx.from)
    |> Enum.map(fn {k, v} -> "#{k} #{v}" end)
    |> lines_or("no settings")
  end

  defp dispatch(["settings", "receipts", value], _opts, ctx) when value in ["on", "off"] do
    settings = ctx.root |> Store.settings(ctx.from) |> Map.put("receipts", value)
    :ok = Store.write_settings(ctx.root, ctx.from, settings)
    {:ok, "receipts #{value}"}
  end

  defp dispatch(["settings", key | _], _opts, _ctx) do
    {:error, "invalid_setting #{key}: receipts on|off"}
  end

  defp dispatch(["whoami"], _opts, ctx), do: {:ok, ctx.from}
  defp dispatch([], _opts, _ctx), do: {:ok, help()}
  defp dispatch(["help"], _opts, _ctx), do: {:ok, help()}
  defp dispatch([cmd | _], _opts, _ctx), do: {:error, "unknown_command #{cmd}"}

  # -- helpers ----------------------------------------------------------------

  defp help do
    """
    dm commands
      conversations [--limit n]
      send <peer> [--reply-to id]     body: two sealed-v1 tokens, one per line
      inbox [--unread] [--since id] [--limit n]
      thread <peer> [--since id] [--limit n] [--bodies]   --bodies marks inbound read
      read <id>
      ack <id>...
      status <id>
      archive <id>...
      react <id> <emoji|none>
      retract <id>                    sender only, within the retract window
      block <peer> | unblock <peer> | blocked
      mute <peer> | unmute <peer> | muted
      settings [receipts on|off]
      whoami
    """
    |> String.trim_trailing()
  end

  defp valid_peer(peer), do: if(Store.pubkey?(peer), do: :ok, else: {:error, "invalid_address"})
  defp valid_id(id), do: if(ULID.valid?(id), do: :ok, else: {:error, "not_found"})

  defp sealed_pair(nil), do: {:error, "unsealed missing body"}

  defp sealed_pair(body) do
    case body |> String.trim() |> String.split("\n") |> Enum.map(&String.trim/1) do
      [to_peer, to_self] ->
        with :ok <- sealed_token(to_peer), :ok <- sealed_token(to_self) do
          {:ok, to_peer, to_self}
        end

      _ ->
        {:error, "unsealed body must be two sealed-v1 tokens, one per line"}
    end
  end

  defp sealed_token(token) do
    cond do
      not Regex.match?(@token_re, token) ->
        {:error, "unsealed"}

      byte_size(token) > Config.max_body_bytes() ->
        {:error, "too_large max #{Config.max_body_bytes()} bytes"}

      true ->
        :ok
    end
  end

  defp receipt_each(ids, ctx, event, allowed?) do
    Enum.reduce_while(ids, :ok, fn id, :ok ->
      with :ok <- valid_id(id),
           {:ok, msg} <- Store.get_message(ctx.root, ctx.from, id),
           true <- allowed?.(msg) or {:error, "forbidden #{id}"} do
        :ok = Store.add_receipt(ctx.root, ctx.from, id, event)
        {:cont, :ok}
      else
        {:error, _} = error -> {:halt, error}
      end
    end)
  end

  # The flags field is `<state>[;reaction=<value>:<by>[,<value>:<by>...]]`.
  defp flags_field(state, []), do: state

  defp flags_field(state, reactions) do
    state <> ";reaction=" <> Enum.map_join(reactions, ",", fn {by, v} -> "#{v}:#{by}" end)
  end

  defp holders(msg), do: Enum.uniq([msg["from"], msg["to"]])

  defp valid_reaction("none"), do: :ok

  defp valid_reaction(value) do
    n = length(String.graphemes(value))

    if n in 1..4 and not String.contains?(value, [" ", "\t", "\n", ",", ";", ":"]) do
      :ok
    else
      {:error, "invalid_reaction one to four characters, no separators"}
    end
  end

  defp within_retract_window?(msg) do
    case DateTime.from_iso8601(msg["t"]) do
      {:ok, sent, _} ->
        DateTime.diff(DateTime.utc_now(), sent, :second) <= Config.retract_window_seconds()

      _ ->
        false
    end
  end

  defp retracted?(msg, index),
    do: MapSet.member?(Map.get(index, msg["id"], MapSet.new()), "retracted")

  defp outbound?(msg, ctx), do: msg["from"] == ctx.from
  defp other(msg, ctx), do: if(outbound?(msg, ctx), do: msg["to"], else: msg["from"])
  defp read?(msg, index), do: MapSet.member?(Map.get(index, msg["id"], MapSet.new()), "read")

  defp archived?(msg, index),
    do: MapSet.member?(Map.get(index, msg["id"], MapSet.new()), "archived")

  defp since(msgs, nil), do: msgs
  defp since(msgs, id), do: Enum.filter(msgs, &(&1["id"] > id))

  defp limit_first(items, nil), do: Enum.take(items, 50)

  defp limit_first(items, n) do
    case Integer.parse(to_string(n)) do
      {n, ""} when n > 0 -> Enum.take(items, n)
      _ -> Enum.take(items, 50)
    end
  end

  defp limit(msgs, nil), do: Enum.take(msgs, -50)

  defp limit(msgs, n) do
    case Integer.parse(to_string(n)) do
      {n, ""} when n > 0 -> Enum.take(msgs, -n)
      _ -> Enum.take(msgs, -50)
    end
  end

  defp line(msg, ctx) do
    dir = if outbound?(msg, ctx), do: "out", else: "in"
    "#{msg["id"]}\t#{dir}\t#{other(msg, ctx)}\t#{msg["t"]}\t#{byte_size(msg["body"])}"
  end

  defp render(msg, retracted, reactions) do
    [
      "id: #{msg["id"]}",
      "from: #{msg["from"]}",
      "to: #{msg["to"]}",
      "t: #{msg["t"]}",
      if(msg["reply_to"], do: "reply_to: #{msg["reply_to"]}"),
      if(retracted, do: "retracted: yes"),
      if(reactions != [],
        do: "reactions: " <> Enum.map_join(reactions, " ", fn {by, v} -> "#{v} #{by}" end)
      ),
      "body: #{msg["body"]}"
    ]
    |> Enum.reject(&is_nil/1)
    |> Enum.join("\n")
  end

  defp maybe_put(map, _k, nil), do: map
  defp maybe_put(map, k, v), do: Map.put(map, k, v)

  defp lines_or([], empty), do: {:ok, empty}
  defp lines_or(lines, _empty), do: {:ok, Enum.join(lines, "\n")}
end
