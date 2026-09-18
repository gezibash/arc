System.delete_env("ARC_RELAY")
System.delete_env("ARC_RELAY_PUBKEY")

ExUnit.start()

defmodule Arc.CLI.TestTeardown do
  @moduledoc false

  # An on_exit callback can run while the test process is still exiting. A
  # process linked to the test process can then die during the stop.
  def stop(pid, timeout \\ :infinity) do
    if is_pid(pid) do
      try do
        GenServer.stop(pid, :normal, timeout)
      catch
        :exit, _ -> :ok
      end
    end

    :ok
  end
end
