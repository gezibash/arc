defmodule Dm.Config do
  @moduledoc "Environment configuration."

  def root do
    (System.get_env("DM_ROOT") || "~/.arc/dm") |> Path.expand()
  end

  @doc "Max bytes of one sealed body token. Default 96 KiB."
  def max_body_bytes do
    case System.get_env("DM_MAX_BODY") do
      nil -> 98_304
      s -> String.to_integer(s)
    end
  end
end
