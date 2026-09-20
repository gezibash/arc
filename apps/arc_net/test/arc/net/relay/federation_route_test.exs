defmodule Arc.Net.Relay.FederationRouteTest do
  use ExUnit.Case, async: true

  alias Arc.Net.Relay.FederationRoute

  test "round trips a compact routed request" do
    route = %{
      packet: <<1, 2, 3>>,
      path: [key(1), key(2), key(3)],
      cursor: 1,
      mode: :request,
      record: record()
    }

    assert {:ok, bytes} = FederationRoute.encode(route)
    assert {:ok, ^route} = FederationRoute.decode(bytes)
  end

  test "rejects malformed routes" do
    assert {:error, :invalid_route} =
             FederationRoute.encode(%{
               packet: <<>>,
               path: [key(1)],
               cursor: 0,
               mode: :reply,
               record: nil
             })

    assert {:error, :invalid_route} =
             FederationRoute.encode(%{
               packet: <<>>,
               path: [key(1), key(1)],
               cursor: 1,
               mode: :reply,
               record: nil
             })

    assert {:error, :invalid_route} =
             FederationRoute.encode(%{
               packet: <<>>,
               path: [key(1), key(2)],
               cursor: 2,
               mode: :reply,
               record: nil
             })

    assert {:error, :invalid_route} =
             FederationRoute.decode(<<1, 1, 1, 2, 0::32, 0::16, key(1)::binary, key(2)::binary>>)
  end

  test "rejects oversized packet and invalid record shape" do
    assert {:error, :invalid_route} =
             FederationRoute.encode(%{
               packet: :binary.copy(<<0>>, 8 * 1024 * 1024 + 1),
               path: [key(1), key(2)],
               cursor: 1,
               mode: :reply,
               record: nil
             })

    assert {:error, :invalid_route} =
             FederationRoute.encode(%{
               packet: <<>>,
               path: [key(1), key(2)],
               cursor: 1,
               mode: :request,
               record: %{}
             })
  end

  test "accepts an exact eight-megabyte routed packet plus route metadata" do
    route = %{
      packet: :binary.copy(<<0>>, 8 * 1024 * 1024),
      path: [key(1), key(2)],
      cursor: 1,
      mode: :reply,
      record: nil
    }

    assert {:ok, bytes} = FederationRoute.encode(route)
    assert byte_size(bytes) > 8 * 1024 * 1024
    assert {:ok, decoded} = FederationRoute.decode(bytes)
    assert byte_size(decoded.packet) == 8 * 1024 * 1024
  end

  defp key(byte), do: :binary.copy(<<byte>>, 32)

  defp record,
    do: %{
      "version" => 1,
      "public_key" => "a",
      "capabilities" => [],
      "issued_at" => 1,
      "expires_at" => 2,
      "signature" => "c"
    }
end
