defmodule Arc.Data.CapabilityManifest do
  @moduledoc """
  ARC-native capability summary and detail documents for serving agents.

  Launch uses a summary-first shape:

  - `GET /info` returns a compact manifest summary
  - `GET /info/capabilities/:id` expands one capability into detail
  """

  alias Arc.Data.CapabilityPackage
  alias Arc.Data.Handler
  alias Arc.Identity

  @manifest_version 2
  @summary_path "/info"
  @detail_prefix "/info/capabilities/"

  @spec request(map()) :: {:summary} | {:detail, String.t()} | :none
  def request(meta) when is_map(meta) do
    method = meta["method"] |> to_string_safe() |> String.upcase()
    path = meta["path"] |> to_string_safe()

    cond do
      method == "GET" and path == @summary_path ->
        {:summary}

      method == "GET" and String.starts_with?(path, @detail_prefix) ->
        capability_id = String.trim_leading(path, @detail_prefix)

        if capability_id != "" and not String.contains?(capability_id, "/") do
          {:detail, capability_id}
        else
          :none
        end

      true ->
        :none
    end
  end

  def request(_meta), do: :none

  @spec summary(Identity.t(), {module(), term()}) :: map()
  def summary(%Identity{} = identity, {mod, state}) do
    package = Handler.package(mod, state)
    capability = package["capability"] || %{}
    signed = CapabilityPackage.sign(identity, package)

    %{
      "manifest_version" => @manifest_version,
      "view" => "summary",
      "provider" => signed["provider"],
      "capability_count" => 1,
      "capabilities" => [summary_capability(capability, signed)]
    }
  end

  @spec detail(Identity.t(), {module(), term()}, String.t()) ::
          {:ok, map()} | {:error, :not_found}
  def detail(%Identity{} = identity, {mod, state}, capability_id) when is_binary(capability_id) do
    package = Handler.package(mod, state)
    capability = package["capability"] || %{}

    if capability["id"] == capability_id do
      signed = CapabilityPackage.sign(identity, package)

      {:ok,
       signed
       |> Map.put("manifest_version", @manifest_version)
       |> Map.put("view", "detail")
       |> Map.update!("capability", &detail_capability/1)}
    else
      {:error, :not_found}
    end
  end

  @spec summary_path() :: String.t()
  def summary_path, do: @summary_path

  @spec detail_path(String.t()) :: String.t()
  def detail_path(capability_id) when is_binary(capability_id),
    do: @detail_prefix <> capability_id

  defp summary_capability(capability, signed) do
    %{
      "id" => capability["id"],
      "kind" => capability["kind"],
      "scheme" => capability["scheme"],
      "title" => capability["title"],
      "summary" => capability["summary"],
      "invocation_mode" => get_in(capability, ["invocation", "mode"]) || "request_reply",
      "release_version" => get_in(signed, ["release", "version"]),
      "channel" => get_in(signed, ["release", "channel"]),
      "detail_path" => detail_path(capability["id"])
    }
  end

  defp detail_capability(capability) do
    capability
    |> Map.put("detail_path", detail_path(capability["id"]))
    |> Map.put("expand", %{"path" => detail_path(capability["id"])})
  end

  defp to_string_safe(nil), do: ""
  defp to_string_safe(value) when is_binary(value), do: value
  defp to_string_safe(value), do: to_string(value)
end
