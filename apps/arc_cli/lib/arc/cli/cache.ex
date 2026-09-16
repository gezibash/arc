defmodule Arc.CLI.Cache do
  @moduledoc """
  An opt-in local cache of opened records, per tool and per identity.

    arc cache on <tool>             start caching
    arc cache off <tool>            stop caching, keep files
    arc cache clear <tool>          delete the cache
    arc cache status <tool>
    arc cache search <tool> <query> case-insensitive search of cached bodies

  A command whose output filters include `cache` stores each record it
  prints. Every cache file is sealed to the identity's own key, so the
  cache on disk is ciphertext. Files live under
  `~/.arc/cache/<tool>/<public key>/<id>.sealed`.
  """

  alias Arc.Data.Render.Conversation
  alias Arc.Data.Toolbox
  alias Arc.Identity
  alias Arc.Identity.SealedBox

  @default_dir Path.join(["~", ".arc", "cache"])

  @doc "The output filter: store every record in `text`, then return it unchanged."
  @spec store(String.t(), Identity.t() | nil, String.t()) :: String.t()
  def store(namespace, %Identity{} = id, text) do
    if enabled?(namespace) do
      {x_pub, _} = Identity.to_x25519(id)
      dir = identity_dir(namespace, id)
      File.mkdir_p!(dir)

      text
      |> String.split("\n")
      |> Enum.drop(1)
      |> Conversation.records()
      |> Enum.map(&Conversation.parse_record/1)
      |> Enum.filter(&is_map/1)
      |> Enum.each(fn r ->
        record = Map.take(r, [:id, :dir, :peer, :t, :reply_to, :body])
        json = record |> Map.new(fn {k, v} -> {Atom.to_string(k), v} end) |> :json.encode()

        File.write!(
          Path.join(dir, r.id <> ".sealed"),
          SealedBox.seal(x_pub, IO.iodata_to_binary(json))
        )
      end)
    end

    text
  end

  def store(_namespace, _id, text), do: text

  def run(["on", namespace]) do
    File.mkdir_p!(namespace_dir(namespace))
    File.write!(flag_path(namespace), "on\n")
    IO.puts("cache on for #{namespace}")
  end

  def run(["off", namespace]) do
    File.rm(flag_path(namespace))
    IO.puts("cache off for #{namespace}")
  end

  def run(["clear", namespace]) do
    File.rm_rf!(namespace_dir(namespace))
    IO.puts("cache cleared for #{namespace}")
  end

  def run(["status", namespace]) do
    state = if enabled?(namespace), do: "on", else: "off"
    count = namespace_dir(namespace) |> Path.join("*/*.sealed") |> Path.wildcard() |> length()
    IO.puts("cache #{state} for #{namespace}, #{count} records")
  end

  def run(["search", namespace | words]) when words != [] do
    query = words |> Enum.join(" ") |> String.downcase()

    with_identity(fn id ->
      hits =
        namespace
        |> records(id)
        |> Enum.filter(&String.contains?(String.downcase(&1["body"]), query))
        |> Enum.sort_by(& &1["id"])

      if hits == [] do
        IO.puts("no matches")
      else
        Enum.each(hits, fn r ->
          line =
            r["body"]
            |> String.split("\n")
            |> Enum.find(&String.contains?(String.downcase(&1), query))

          IO.puts(Toolbox.petnames("#{r["id"]}\t#{r["dir"]}\t#{r["peer"]}\t#{r["t"]}\t#{line}"))
        end)
      end
    end)
  end

  def run(_) do
    IO.puts("""
    arc cache commands

      cache on|off|clear|status <tool>
      cache search <tool> <query>
    """)
  end

  @doc "Every cached record for `namespace`, opened with `id`."
  @spec records(String.t(), Identity.t()) :: [map()]
  def records(namespace, %Identity{} = id) do
    namespace
    |> identity_dir(id)
    |> Path.join("*.sealed")
    |> Path.wildcard()
    |> Enum.flat_map(fn path ->
      with {:ok, sealed} <- File.read(path),
           {:ok, json} <- SealedBox.open(id, sealed) do
        [:json.decode(json)]
      else
        _ -> []
      end
    end)
  end

  def enabled?(namespace), do: File.exists?(flag_path(namespace))

  defp with_identity(fun) do
    case Identity.KeyStore.resolve_active() do
      {:ok, id} ->
        fun.(id)

      {:error, reason} ->
        IO.puts(:stderr, "error: #{Arc.CLI.Keys.describe_error(reason)}")
        Arc.CLI.Exit.halt(1)
    end
  end

  defp dir, do: Application.get_env(:arc_cli, :cache_dir, @default_dir)
  defp namespace_dir(namespace), do: Path.expand(Path.join(dir(), namespace))
  defp flag_path(namespace), do: Path.join(namespace_dir(namespace), "enabled")

  defp identity_dir(namespace, id),
    do: Path.join(namespace_dir(namespace), Identity.encode_public_key(id))
end
