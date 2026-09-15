defmodule Dm.ULID do
  @moduledoc """
  26 character ULID: 48 bit millisecond timestamp, 80 random bits, Crockford
  base32. Ids sort by time. Within one process, ids generated in the same
  millisecond are strictly increasing.
  """

  @alphabet ~c"0123456789ABCDEFGHJKMNPQRSTVWXYZ"

  @spec generate() :: String.t()
  def generate do
    now = System.system_time(:millisecond)

    {ts, rand} =
      case Process.get(__MODULE__) do
        {^now, last} -> {now, last + 1}
        _ -> {now, :binary.decode_unsigned(:crypto.strong_rand_bytes(10))}
      end

    Process.put(__MODULE__, {ts, rand})
    encode(<<ts::48, rand::80>>)
  end

  @spec valid?(term()) :: boolean()
  def valid?(id) when is_binary(id), do: Regex.match?(~r/^[0-9A-HJKMNP-TV-Z]{26}$/, id)
  def valid?(_), do: false

  # 128 bits read as one integer, emitted as 26 five-bit groups. The top
  # two bits of the first group are always zero.
  defp encode(<<n::128>>) do
    for shift <- Enum.to_list(125..0//-5), into: "" do
      <<Enum.at(@alphabet, Bitwise.band(Bitwise.bsr(n, shift), 31))>>
    end
  end
end
