defmodule Arc.CLIRelayConfigTest do
  use ExUnit.Case, async: false

  setup do
    original = System.get_env("ARC_RELAY_KEY")
    System.delete_env("ARC_RELAY_KEY")

    on_exit(fn ->
      if original,
        do: System.put_env("ARC_RELAY_KEY", original),
        else: System.delete_env("ARC_RELAY_KEY")
    end)

    :ok
  end

  test "invalid or incomplete peer configuration fails before starting a listener" do
    for peer <- [
          "localhost:7331",
          "not-a-key@localhost:7331",
          String.duplicate("f", 64) <> "@bad"
        ] do
      {result, stderr} =
        ExUnit.CaptureIO.with_io(:stderr, fn ->
          Arc.CLI.main(["relay", "--peer", peer])
        end)

      assert result == {:exit, 1}
      assert stderr =~ "invalid --peer"
    end
  end

  test "federation refuses an ephemeral local relay identity" do
    peer = Arc.Identity.generate() |> Arc.Identity.encode_public_key()

    {result, stderr} =
      ExUnit.CaptureIO.with_io(:stderr, fn ->
        Arc.CLI.main(["relay", "--peer", peer <> "@localhost:7331"])
      end)

    assert result == {:exit, 1}
    assert stderr =~ "federation requires a persistent relay identity"
  end

  test "unknown relay flags are rejected rather than silently omitting peering" do
    {result, stderr} =
      ExUnit.CaptureIO.with_io(:stderr, fn ->
        Arc.CLI.main(["relay", "--peers", "mistyped"])
      end)

    assert result == {:exit, 1}
    assert stderr =~ "invalid relay options"
  end

  test "transit also requires a persistent operator identity before opening a listener" do
    {result, stderr} =
      ExUnit.CaptureIO.with_io(:stderr, fn ->
        Arc.CLI.main(["relay", "--transit"])
      end)

    assert result == {:exit, 1}
    assert stderr =~ "federation requires a persistent relay identity"
  end
end
