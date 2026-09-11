defmodule Journal.Config do
  @moduledoc """
  Environment configuration.

  `max_blob_bytes/0` is the attachment cap. It is a storage budget, not a
  transport limit. Blobs live in `blobs/` outside git and v1 does not back
  them up, so the cap keeps one `attach` from filling the data directory.

  The cap was 512 KiB while the ARC exec port cut provider reply lines at
  1 MB, so that a base64 `fetch` reply would fit on one line. That line cap
  is gone. The exec port now joins stdout chunks up to 64 MiB per line, and a
  16 MiB blob is 21.4 MiB as base64, well inside it.
  """

  def root do
    (System.get_env("JOURNAL_ROOT") || "~/.arc/journal") |> Path.expand()
  end

  def remote, do: System.get_env("JOURNAL_REMOTE")

  @doc "Max attachment bytes. A storage budget, see the moduledoc."
  def max_blob_bytes, do: 16 * 1024 * 1024
end
