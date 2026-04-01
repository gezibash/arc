defmodule Arc.Data.CapabilityPackage do
  @moduledoc """
  Canonical ARC capability packages.

  Providers author package documents in JSON or TOML. ARC normalizes those
  documents, signs them with the serving identity, and serves the signed result
  as the installable package contract for a remote capability.
  """

  alias Arc.Data.InterfaceManifest
  alias Arc.Identity

  @package_version 1
  @default_release_version "0.1.0"
  @default_channel "stable"

  @spec load_file(String.t()) :: {:ok, map()} | {:error, term()}
  def load_file(path) when is_binary(path) and path != "" do
    with {:ok, body} <- File.read(path),
         {:ok, parsed} <- decode(path, body),
         {:ok, package} <- normalize_document(parsed) do
      {:ok, package}
    end
  end

  def load_file(_path), do: {:error, :invalid_capability_package}

  @spec normalize_package(map()) :: map()
  def normalize_package(package) when is_map(package) do
    %{
      "package_version" => @package_version,
      "capability" => normalize_capability(Map.get(package, "capability", %{})),
      "release" => normalize_release(Map.get(package, "release"))
    }
    |> maybe_put("published_at", present_string(Map.get(package, "published_at")))
  end

  def normalize_package(_package), do: %{}

  @spec wrap_capability(map()) :: map()
  def wrap_capability(capability) when is_map(capability) do
    normalize_package(%{"capability" => capability})
  end

  @spec normalize_capability(map()) :: map()
  def normalize_capability(capability) when is_map(capability) do
    interfaces =
      capability
      |> Map.get("interfaces")
      |> InterfaceManifest.normalize()
      |> merge_interfaces(InterfaceManifest.from_legacy_cli(Map.get(capability, "cli")))

    %{
      "id" => Map.get(capability, "id", "primary"),
      "kind" => Map.get(capability, "kind"),
      "scheme" => Map.get(capability, "scheme"),
      "title" => Map.get(capability, "title"),
      "summary" => Map.get(capability, "summary"),
      "invocation" => normalize_invocation(Map.get(capability, "invocation", %{})),
      "examples" => normalize_examples(Map.get(capability, "examples", []))
    }
    |> maybe_put("config", normalize_config(Map.get(capability, "config")))
    |> maybe_put("interfaces", interfaces)
  end

  def normalize_capability(_capability), do: normalize_capability(%{})

  @spec valid?(map()) :: boolean()
  def valid?(%{"capability" => capability, "release" => release} = package)
      when is_map(capability) and is_map(release) do
    capability_valid?(capability) and
      not is_nil(present_string(Map.get(release, "version"))) and
      not is_nil(present_string(Map.get(release, "channel"))) and
      package["package_version"] == @package_version
  end

  def valid?(_package), do: false

  @spec sign(Identity.t(), map()) :: map()
  def sign(%Identity{} = identity, package) when is_map(package) do
    package = normalize_package(package)
    provider = provider(identity)
    payload = canonical_payload(Map.merge(package, %{"provider" => provider}))
    package_hash = hash_binary(payload)
    signature = Identity.sign(identity, payload)

    package
    |> Map.put("provider", provider)
    |> Map.put("package_hash", Base.encode16(package_hash, case: :lower))
    |> Map.put("signature", %{
      "algorithm" => "ed25519",
      "signer_public_key" => provider["public_key"],
      "value" => Base.encode16(signature, case: :lower)
    })
  end

  @spec verify(map()) :: {:ok, map()} | {:error, term()}
  def verify(%{} = detail) do
    package = normalize_signed_package(detail)

    with true <- valid_signed_package?(package) or {:error, :invalid_capability_package},
         {:ok, provider_pk} <- decode_hex(get_in(package, ["provider", "public_key"])),
         {:ok, signer_pk} <- decode_hex(get_in(package, ["signature", "signer_public_key"])),
         true <- provider_pk == signer_pk or {:error, :signer_mismatch},
         {:ok, signature} <- decode_hex(get_in(package, ["signature", "value"])),
         payload <- canonical_payload(package),
         expected_hash <- Base.encode16(hash_binary(payload), case: :lower),
         true <- expected_hash == package["package_hash"] or {:error, :hash_mismatch},
         true <- Identity.verify(provider_pk, payload, signature) or {:error, :invalid_signature} do
      {:ok, package}
    else
      false -> {:error, :invalid_capability_package}
      {:error, _reason} = error -> error
    end
  end

  def verify(_detail), do: {:error, :invalid_capability_package}

  @spec diff(map(), map()) :: [String.t()]
  def diff(local, remote) when is_map(local) and is_map(remote) do
    []
    |> diff_field("package hash", local["package_hash"], remote["package_hash"])
    |> diff_field(
      "release version",
      get_in(local, ["release", "version"]),
      get_in(remote, ["release", "version"])
    )
    |> diff_field(
      "channel",
      get_in(local, ["release", "channel"]),
      get_in(remote, ["release", "channel"])
    )
    |> diff_field("published at", local["published_at"], remote["published_at"])
    |> diff_field(
      "title",
      get_in(local, ["capability", "title"]),
      get_in(remote, ["capability", "title"])
    )
    |> diff_field(
      "summary",
      get_in(local, ["capability", "summary"]),
      get_in(remote, ["capability", "summary"])
    )
    |> diff_field(
      "invocation mode",
      get_in(local, ["capability", "invocation", "mode"]),
      get_in(remote, ["capability", "invocation", "mode"])
    )
    |> diff_field("namespace", cli_namespace(local), cli_namespace(remote))
    |> diff_field(
      "commands",
      Enum.join(command_labels(local), ", "),
      Enum.join(command_labels(remote), ", ")
    )
  end

  def provider(%Identity{} = identity) do
    %{
      "name" => Identity.name(identity),
      "short_name" => Identity.short_name(identity),
      "public_key" => Identity.encode_public_key(identity)
    }
  end

  @spec canonical_payload(map()) :: binary()
  def canonical_payload(%{} = package) do
    payload =
      package
      |> normalize_package()
      |> Map.put("provider", Map.get(package, "provider"))

    encode_canonical(payload)
  end

  defp normalize_document(%{"capability" => capability} = document) when is_map(capability) do
    interfaces =
      document
      |> Map.get("interfaces")
      |> InterfaceManifest.normalize()

    capability =
      capability
      |> merge_optional("interfaces", interfaces)
      |> merge_optional("examples", Map.get(document, "examples"))
      |> normalize_capability()

    package =
      %{
        "package_version" => @package_version,
        "capability" => capability,
        "release" => normalize_release(Map.get(document, "release"))
      }
      |> maybe_put(
        "published_at",
        present_string(
          Map.get(document, "published_at") || get_in(document, ["release", "published_at"])
        )
      )

    if valid?(package), do: {:ok, package}, else: {:error, :invalid_capability_package}
  end

  defp normalize_document(_document), do: {:error, :invalid_capability_package}

  defp decode(path, body) do
    case Path.extname(path) do
      ".json" ->
        try do
          {:ok, :json.decode(body)}
        rescue
          _ -> {:error, :invalid_json}
        end

      ".toml" ->
        TomlElixir.decode(body)

      _ ->
        {:error, :unsupported_capability_format}
    end
  end

  defp normalize_signed_package(package) do
    package
    |> normalize_package()
    |> Map.put("provider", Map.get(package, "provider"))
    |> Map.put("package_hash", Map.get(package, "package_hash"))
    |> Map.put("signature", Map.get(package, "signature"))
  end

  defp valid_signed_package?(package) do
    valid?(package) and
      is_map(Map.get(package, "provider")) and
      not is_nil(present_string(Map.get(package, "package_hash"))) and
      match?(
        %{"algorithm" => "ed25519", "signer_public_key" => _, "value" => _},
        Map.get(package, "signature")
      )
  end

  defp capability_valid?(%{
         "id" => id,
         "kind" => kind,
         "scheme" => scheme,
         "title" => title,
         "summary" => summary,
         "invocation" => %{"method" => method, "path" => path}
       }) do
    Enum.all?([id, kind, scheme, title, summary, method, path], &present_string/1)
  end

  defp capability_valid?(_capability), do: false

  defp normalize_invocation(invocation) when is_map(invocation) do
    %{
      "mode" => normalize_invocation_mode(Map.get(invocation, "mode")),
      "method" => Map.get(invocation, "method", "RAW"),
      "path" => Map.get(invocation, "path", "/"),
      "request_body" =>
        Map.get(invocation, "request_body", %{
          "type" => "text",
          "description" => "Plain text request body"
        }),
      "response_body" =>
        Map.get(invocation, "response_body", %{
          "type" => "text",
          "description" => "Plain text response body"
        })
    }
    |> maybe_put("stream", normalize_stream_invocation(Map.get(invocation, "stream")))
  end

  defp normalize_invocation(_invocation), do: normalize_invocation(%{})

  defp normalize_stream_invocation(stream) when is_map(stream) do
    operations =
      case Map.get(stream, "operations") do
        list when is_list(list) ->
          list
          |> Enum.filter(&is_binary/1)
          |> Enum.map(&String.downcase/1)
          |> Enum.uniq()

        _ ->
          ["open", "data", "resize", "close", "exit", "error"]
      end

    %{}
    |> Map.put("operations", operations)
    |> maybe_put("tty", normalize_boolean(Map.get(stream, "tty")))
    |> maybe_put("encoding", present_string(Map.get(stream, "encoding")))
    |> maybe_put("input", normalize_stream_io(Map.get(stream, "input")))
    |> maybe_put("output", normalize_stream_io(Map.get(stream, "output")))
  end

  defp normalize_stream_invocation(_stream), do: nil

  defp normalize_stream_io(io) when is_map(io) do
    %{}
    |> maybe_put("type", present_string(Map.get(io, "type")))
    |> maybe_put("description", present_string(Map.get(io, "description")))
  end

  defp normalize_stream_io(_io), do: nil

  defp normalize_invocation_mode("stream"), do: "stream"
  defp normalize_invocation_mode(_mode), do: "request_reply"

  defp normalize_boolean(value) when value in [true, false], do: value
  defp normalize_boolean(_value), do: nil

  defp normalize_release(release) when is_map(release) do
    %{
      "version" => present_string(Map.get(release, "version")) || @default_release_version,
      "channel" => present_string(Map.get(release, "channel")) || @default_channel
    }
  end

  defp normalize_release(_release) do
    %{
      "version" => @default_release_version,
      "channel" => @default_channel
    }
  end

  defp normalize_examples(examples) when is_list(examples) do
    Enum.filter(examples, &is_binary/1)
  end

  defp normalize_examples(_examples), do: []

  defp normalize_config(config) when is_map(config) and map_size(config) > 0, do: config
  defp normalize_config(_config), do: nil

  defp merge_interfaces(nil, nil), do: nil
  defp merge_interfaces(%{} = interfaces, nil), do: interfaces
  defp merge_interfaces(nil, %{} = interfaces), do: interfaces
  defp merge_interfaces(%{} = interfaces, %{} = legacy), do: Map.merge(legacy, interfaces)

  defp merge_optional(map, _key, nil), do: map
  defp merge_optional(map, key, value), do: Map.put_new(map, key, value)

  defp maybe_put(map, _key, nil), do: map
  defp maybe_put(map, key, value), do: Map.put(map, key, value)

  defp present_string(value) when is_binary(value) and value != "", do: value
  defp present_string(_value), do: nil

  defp hash_binary(payload), do: :crypto.hash(:sha256, payload)

  defp decode_hex(value) when is_binary(value) do
    case Base.decode16(value, case: :mixed) do
      {:ok, binary} -> {:ok, binary}
      :error -> {:error, :invalid_hex}
    end
  end

  defp decode_hex(_value), do: {:error, :invalid_hex}

  defp encode_canonical(value) when is_map(value) do
    body =
      value
      |> Map.keys()
      |> Enum.sort()
      |> Enum.map(fn key ->
        [encode_scalar(key), ?:, encode_canonical(Map.get(value, key))]
      end)
      |> Enum.intersperse(?,)

    IO.iodata_to_binary([?{, body, ?}])
  end

  defp encode_canonical(value) when is_list(value) do
    body =
      value
      |> Enum.map(&encode_canonical/1)
      |> Enum.intersperse(?,)

    IO.iodata_to_binary([?[, body, ?]])
  end

  defp encode_canonical(value), do: encode_scalar(value)

  defp encode_scalar(value), do: IO.iodata_to_binary(:json.encode(value))

  defp diff_field(changes, _label, left, right) when left == right, do: changes

  defp diff_field(changes, label, left, right) do
    changes ++ ["#{label}: #{format_value(left)} -> #{format_value(right)}"]
  end

  defp format_value(nil), do: "(none)"
  defp format_value(""), do: "(empty)"
  defp format_value(value), do: to_string(value)

  defp cli_namespace(detail) do
    detail
    |> Map.get("capability", %{})
    |> InterfaceManifest.cli()
    |> case do
      %{"namespace" => namespace} -> namespace
      _ -> nil
    end
  end

  defp command_labels(detail) do
    detail
    |> Map.get("capability", %{})
    |> InterfaceManifest.cli()
    |> case do
      %{"commands" => commands} when is_list(commands) ->
        Enum.map(commands, fn command ->
          case Map.get(command, "path", []) do
            [] -> "(root)"
            path -> Enum.join(path, " ")
          end
        end)

      _ ->
        []
    end
  end
end
