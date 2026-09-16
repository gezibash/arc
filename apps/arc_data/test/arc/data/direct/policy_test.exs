defmodule Arc.Data.Direct.PolicyTest do
  use ExUnit.Case, async: true
  alias Arc.Data.Direct.Policy

  @rule %{
    "peer" => String.duplicate("ab", 32),
    "capability" => "primary",
    "scheme" => "sqlite",
    "path" => "/main",
    "lease_ms" => 30_000,
    "dial" => ["127.0.0.1", "::1"]
  }

  test "owner approvals bind exact full keys, resources, and literal addresses" do
    assert {:ok, [rule]} = Policy.normalize([@rule])
    assert rule.hole_punch == false

    assert {:ok, {127, 0, 0, 1}, 5000} =
             Policy.candidate(rule, %{"host" => "127.0.0.1", "port" => 5000})

    assert {:error, :direct_address_denied} =
             Policy.candidate(rule, %{"host" => "localhost", "port" => 5000})

    assert {:error, :direct_address_denied} =
             Policy.candidate(rule, %{"host" => "127.0.0.2", "port" => 5000})

    assert {:error, :direct_address_denied} =
             Policy.candidate(rule, %{"host" => "127.0.0.1", "port" => 0})

    assert nil ==
             Policy.find([rule], rule.peer, %{
               "capability" => "primary",
               "scheme" => "sqlite",
               "path" => "/other"
             })

    assert {:ok, [^rule]} = Policy.normalize([rule])
  end

  test "hole punching is an explicit opt-in with an exact dial allowlist" do
    assert {:ok, [rule]} = Policy.normalize([Map.put(@rule, "hole_punch", true)])
    assert rule.hole_punch == true
    assert rule.dial == [{127, 0, 0, 1}, {0, 0, 0, 0, 0, 0, 0, 1}]
    assert {:ok, [^rule]} = Policy.normalize([rule])

    assert {:error, :invalid_direct_policy} =
             Policy.normalize([
               @rule
               |> Map.put("dial", [])
               |> Map.put("hole_punch", true)
               |> Map.put("listen", %{"bind" => "0.0.0.0", "address" => "192.0.2.7", "port" => 0})
             ])

    for value <- ["true", 1, nil] do
      assert {:error, :invalid_direct_policy} =
               Policy.normalize([Map.put(@rule, "hole_punch", value)])
    end
  end

  test "normalizes existing programmatic seven-key rules with a false hole-punch default" do
    assert {:ok, [rule]} = Policy.normalize([@rule])

    existing_normalized = Map.delete(rule, :hole_punch)
    assert map_size(existing_normalized) == 7
    assert {:ok, [renormalized]} = Policy.normalize([existing_normalized])
    assert renormalized.hole_punch == false
    assert renormalized == rule
  end

  test "rejects misspelled settings, wildcard scope, unsafe destinations, and unlimited leases" do
    invalid = [
      Map.put(@rule, "allow_all", true),
      Map.put(@rule, "peer", "*"),
      Map.put(@rule, "path", "/*"),
      Map.put(@rule, "path", ""),
      Map.put(@rule, "path", "/../main"),
      Map.put(@rule, "lease_ms", 0),
      Map.put(@rule, "lease_ms", 120_001),
      Map.put(@rule, "dial", ["169.254.169.254"]),
      Map.put(@rule, "dial", ["224.0.0.1"]),
      Map.put(@rule, "dial", ["::ffff:169.254.169.254"]),
      Map.put(@rule, "dial", ["fe80::1"]),
      Map.put(@rule, "dial", ["0.0.0.0"])
    ]

    for rule <- invalid, do: assert({:error, :invalid_direct_policy} = Policy.normalize([rule]))
    assert {:error, :invalid_direct_policy} = Policy.normalize([@rule, @rule])
    assert {:error, :invalid_direct_policy} = Policy.normalize(List.duplicate(@rule, 33))
  end

  test "listener disclosure must be explicit and same IP family as its bind address" do
    listener = %{"bind" => "0.0.0.0", "address" => "192.0.2.7", "port" => 0}
    assert {:ok, [rule]} = Policy.normalize([Map.put(@rule, "listen", listener)])
    assert rule.listen.ip == {0, 0, 0, 0}
    assert rule.listen.host == {192, 0, 2, 7}

    assert {:error, :invalid_direct_policy} =
             Policy.normalize([Map.put(@rule, "listen", Map.put(listener, "address", "::1"))])
  end

  test "settings version and overall shape are checked" do
    body = :json.encode(%{"version" => 1, "rules" => [@rule]}) |> IO.iodata_to_binary()
    assert {:ok, [_]} = Policy.decode(body)
    assert {:error, :invalid_direct_policy} = Policy.decode("{bad")
    assert {:error, :invalid_direct_policy} = Policy.decode(~s({"version":2,"rules":[]}))

    assert {:error, :invalid_direct_policy} =
             Policy.decode(~s({"version":1,"rules":[],"allow":true}))
  end
end
