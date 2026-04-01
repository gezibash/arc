defmodule Arc.Data.Mailbox do
  @moduledoc """
  File-based mailbox for cross-process message delivery.

  Each agent's mailbox is a directory at ~/.config/arc/mailbox/<pubkey_hex>/.
  Packets are written as raw binary files and polled by the listener.
  """

  @mailbox_root Path.join(["~", ".config", "arc", "mailbox"])

  @doc """
  Deliver a raw packet binary to a peer's file-based mailbox.
  """
  @spec deliver(binary(), binary()) :: :ok
  def deliver(to_pk, packet) when is_binary(to_pk) and is_binary(packet) do
    dir = mailbox_dir(to_pk)
    File.mkdir_p!(dir)

    filename = "#{System.unique_integer([:positive, :monotonic])}.packet"
    File.write!(Path.join(dir, filename), packet)
    :ok
  end

  @doc """
  Read and clear all packets from a mailbox. Returns list of raw packet binaries.
  """
  @spec read(binary()) :: [binary()]
  def read(public_key) do
    dir = mailbox_dir(public_key)

    case File.ls(dir) do
      {:ok, files} ->
        files
        |> Enum.filter(&String.ends_with?(&1, ".packet"))
        |> Enum.sort()
        |> Enum.flat_map(fn file ->
          path = Path.join(dir, file)

          case File.read(path) do
            {:ok, data} ->
              File.rm!(path)
              [data]

            _ ->
              []
          end
        end)

      {:error, :enoent} ->
        []
    end
  end

  defp mailbox_dir(public_key) do
    hex = Base.encode16(public_key, case: :lower)
    Path.expand(Path.join(@mailbox_root, hex))
  end
end
