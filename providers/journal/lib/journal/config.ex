defmodule Journal.Config do
  @moduledoc "Environment configuration."

  def root do
    (System.get_env("JOURNAL_ROOT") || "~/.arc/journal") |> Path.expand()
  end

  def remote, do: System.get_env("JOURNAL_REMOTE")

  @doc "Max attachment bytes. Replies cap at 1 MB on the exec port, base64 inflates by 4/3."
  def max_blob_bytes, do: 512 * 1024
end
