defmodule Arc.CLI.Update.Manifest do
  @moduledoc """
  Signed, immutable ARC release-channel metadata.

  This module only establishes publisher authority and update eligibility. It
  does not fetch an artifact, stage a release, or apply an OTP upgrade.

  Both versions use a small, closed JSON-shaped schema. All map
  keys are strings, unknown keys are rejected, and the signature covers the
  deterministic JSON encoding of the unsigned document prefixed by the domain
  separator `ARC-RELEASE-CHANNEL-V1\\0` or `ARC-RELEASE-CHANNEL-V2\\0`.
  Version two also permits a restart-only release without hot-upgrade sources.
  Its `install` object must name the same archive as `sha256` and `size`.

  Public API:

    * `sign/2` signs a valid unsigned manifest for test and publisher tooling.
    * `verify/2` verifies the pinned publisher, channel, signature, expiry,
      and monotonic publication sequence.
    * `select/2` returns the newest advertised platform release and the newest
      operator-applicable hot-update edge. It never infers an upgrade from
      version numbers alone or selects a lower SemVer target.
    * `select_install/2` returns the newest eligible complete installation
      archive for a local CLI installation. A release advertises one through
      its optional `install` object, which names the full release archive
      separately from the hot-update package.

  A pin is an exact immutable release build identifier. Selecting a channel
  with a pin suppresses advancement; callers still receive the advertised
  latest release for status output.
  """

  alias Arc.Identity

  @schema_versions [1, 2]
  @domain "ARC-RELEASE-CHANNEL-V"
  @channels ["stable", "beta"]
  @sha256_hex_bytes 64
  @public_key_hex_bytes 64

  @type verified :: %{manifest: map(), digest: String.t()}

  @doc "Signs a complete, unsigned supported channel manifest."
  @spec sign(Identity.t(), map()) :: {:ok, map()} | {:error, atom()}
  def sign(%Identity{} = identity, unsigned) when is_map(unsigned) do
    with :ok <- validate_unsigned(unsigned),
         true <-
           unsigned["publisher"] == Identity.encode_public_key(identity) or
             {:error, :publisher_mismatch} do
      signature =
        unsigned
        |> signing_payload()
        |> then(&Identity.sign(identity, &1))
        |> Base.encode16(case: :lower)

      {:ok, Map.put(unsigned, "signature", %{"algorithm" => "ed25519", "value" => signature})}
    else
      {:error, _reason} = error -> error
    end
  end

  def sign(_identity, _unsigned), do: {:error, :invalid_manifest}

  @doc """
  Verifies a signed manifest against an explicit publisher and channel pin.

  Options are `:expected_publisher`, `:expected_channel`, `:last_sequence`,
  `:last_digest`, and `:now` (Unix seconds). Equal publication sequences are
  accepted only when the canonical unsigned bytes have the same SHA-256 digest.
  """
  @spec verify(map(), keyword()) :: {:ok, verified()} | {:error, atom()}
  def verify(document, opts) when is_map(document) and is_list(opts) do
    with {:ok, unsigned, signature} <- split_signed(document),
         :ok <- validate_unsigned(unsigned),
         :ok <- validate_signature(signature),
         {:ok, publisher} <- decode_hex(unsigned["publisher"], 32),
         :ok <- verify_expected(unsigned, opts),
         payload <- signing_payload(unsigned),
         {:ok, signature_bytes} <- decode_hex(signature["value"], 64),
         true <-
           Identity.verify(publisher, payload, signature_bytes) or {:error, :invalid_signature},
         digest <- digest(unsigned),
         :ok <- verify_expiry(unsigned, opts),
         :ok <- verify_sequence(unsigned["sequence"], digest, opts) do
      {:ok, %{manifest: unsigned, digest: digest}}
    else
      {:error, _reason} = error -> error
    end
  end

  def verify(_document, _opts), do: {:error, :invalid_manifest}

  @doc """
  Selects channel metadata for a running service without applying an update.

  Required options identify the installed artifact exactly: `:installed_build`,
  `:installed_version`, `:installed_runtime`, and `:platform` (`%{"os" => _,
  "arch" => _}`). An optional `:pin` is an exact release build.
  """
  @spec select(verified(), keyword()) :: {:ok, map()} | {:error, atom()}
  def select(%{manifest: manifest} = verified, opts) when is_map(manifest) and is_list(opts) do
    with {:ok, installed} <- installed_context(opts),
         true <- is_binary(verified[:digest]) or {:error, :invalid_verified_manifest} do
      platform_releases =
        manifest["releases"]
        |> Enum.reject(& &1["withdrawn"])
        |> Enum.filter(&(&1["platform"] == installed.platform))
        |> newest_first()

      latest = List.first(platform_releases)
      pin = Keyword.get(opts, :pin)

      result = %{
        latest: latest,
        eligible: nil,
        status: selection_status(latest, installed, pin),
        reason: selection_reason(latest, installed, pin),
        pin: pin
      }

      if is_nil(pin) do
        case Enum.find_value(platform_releases, &eligible_edge(&1, installed)) do
          nil ->
            {:ok, result}

          edge ->
            {:ok, %{result | eligible: edge, status: :available, reason: :compatible_hot_edge}}
        end
      else
        {:ok, result}
      end
    else
      {:error, _reason} = error -> error
    end
  end

  def select(_verified, _opts), do: {:error, :invalid_verified_manifest}

  @doc """
  Selects the newest complete installation archive for a local installation.

  Required options are `:installed_version` and `:platform`. Only releases
  that are eligible, not withdrawn, and carry an `install` object qualify.
  The result reports the channel's latest platform release separately, so an
  installation is never called up to date merely because the newest release
  ships without a complete archive.
  """
  @spec select_install(verified(), keyword()) :: {:ok, map()} | {:error, atom()}
  def select_install(%{manifest: manifest} = verified, opts)
      when is_map(manifest) and is_list(opts) do
    platform = Keyword.get(opts, :platform)

    with {:ok, installed} <- parse_installed_version(Keyword.get(opts, :installed_version)),
         :ok <- validate_platform(platform),
         true <- is_binary(verified[:digest]) or {:error, :invalid_verified_manifest} do
      platform_releases =
        manifest["releases"]
        |> Enum.reject(& &1["withdrawn"])
        |> Enum.filter(&(&1["platform"] == platform))
        |> newest_first()

      latest = List.first(platform_releases)
      candidate = Enum.find(platform_releases, &(&1["eligible"] and is_map(&1["install"])))
      {status, reason} = install_status(latest, candidate, installed)

      {:ok,
       %{
         latest: latest,
         install: if(status == :available, do: candidate),
         status: status,
         reason: reason
       }}
    else
      {:error, _reason} = error -> error
    end
  end

  def select_install(_verified, _opts), do: {:error, :invalid_verified_manifest}

  defp split_signed(document) do
    expected =
      [
        "schema_version",
        "channel",
        "publisher",
        "sequence",
        "expires_at",
        "releases",
        "signature"
      ]

    if Enum.sort(Map.keys(document)) == Enum.sort(expected) do
      {:ok, Map.delete(document, "signature"), document["signature"]}
    else
      {:error, :invalid_manifest}
    end
  end

  defp validate_unsigned(unsigned) when is_map(unsigned) do
    expected =
      ["schema_version", "channel", "publisher", "sequence", "expires_at", "releases"]

    with true <-
           Enum.sort(Map.keys(unsigned)) == Enum.sort(expected) or {:error, :invalid_manifest},
         true <- unsigned["schema_version"] in @schema_versions or {:error, :unsupported_schema},
         true <- unsigned["channel"] in @channels or {:error, :invalid_channel},
         true <-
           valid_hex?(unsigned["publisher"], @public_key_hex_bytes) or
             {:error, :invalid_publisher},
         true <- positive_integer?(unsigned["sequence"]) or {:error, :invalid_sequence},
         true <- positive_integer?(unsigned["expires_at"]) or {:error, :invalid_expiry},
         true <-
           (is_list(unsigned["releases"]) and unsigned["releases"] != []) or
             {:error, :invalid_releases},
         :ok <-
           validate_releases(
             unsigned["releases"],
             unsigned["channel"],
             unsigned["schema_version"]
           ) do
      :ok
    else
      {:error, _reason} = error -> error
    end
  end

  defp validate_releases(releases, channel, schema) do
    with :ok <- validate_all(releases, &validate_release(&1, channel, schema)),
         identities <- Enum.map(releases, &{&1["build"], &1["platform"]}),
         true <-
           length(identities) == MapSet.size(MapSet.new(identities)) or
             {:error, :duplicate_release} do
      :ok
    else
      {:error, _reason} = error -> error
    end
  end

  defp validate_release(release, channel, schema) when is_map(release) do
    with :ok <- validate_release_shape(release),
         :ok <- validate_release_identity(release, channel),
         :ok <- validate_release_artifact(release, schema),
         :ok <- validate_sources(release["sources"]) do
      :ok
    else
      {:error, _reason} = error -> error
    end
  end

  defp validate_release(_release, _channel, _schema), do: {:error, :invalid_release}

  @release_keys Enum.sort([
                  "version",
                  "build",
                  "runtime",
                  "platform",
                  "size",
                  "sha256",
                  "sources",
                  "restart_required",
                  "withdrawn",
                  "eligible"
                ])

  defp validate_release_shape(release) do
    keys = Enum.sort(Map.keys(release))

    cond do
      keys == @release_keys -> :ok
      keys == Enum.sort(["install" | @release_keys]) -> validate_install(release["install"])
      true -> {:error, :invalid_release}
    end
  end

  # The optional complete archive is a plain artifact reference. Its platform
  # and version are the enclosing release's; it never carries hot-edge data.
  defp validate_install(%{"size" => size, "sha256" => sha256} = install)
       when map_size(install) == 2 do
    if positive_integer?(size) and valid_hex?(sha256, @sha256_hex_bytes),
      do: :ok,
      else: {:error, :invalid_install}
  end

  defp validate_install(_), do: {:error, :invalid_install}

  defp validate_release_identity(release, channel) do
    with true <- valid_string?(release["version"], 128) or {:error, :invalid_version},
         {:ok, version} <- parse_version(release["version"]),
         true <- channel != "stable" or version.pre == [] or {:error, :stable_prerelease},
         true <- valid_string?(release["build"], 256) or {:error, :invalid_build},
         true <- valid_string?(release["runtime"], 256) or {:error, :invalid_runtime},
         :ok <- validate_platform(release["platform"]) do
      :ok
    else
      {:error, _reason} = error -> error
      :error -> {:error, :invalid_version}
    end
  end

  defp validate_release_artifact(release, schema) do
    with true <- positive_integer?(release["size"]) or {:error, :invalid_size},
         true <- valid_hex?(release["sha256"], @sha256_hex_bytes) or {:error, :invalid_sha256},
         true <- is_boolean(release["restart_required"]) or {:error, :invalid_restart_required},
         true <- is_boolean(release["withdrawn"]) or {:error, :invalid_withdrawn},
         true <- is_boolean(release["eligible"]) or {:error, :invalid_eligible},
         true <-
           (is_list(release["sources"]) and
              (release["sources"] != [] or
                 (schema == 2 and release["restart_required"] and
                    release["install"] == Map.take(release, ["sha256", "size"])))) or
             {:error, :invalid_sources} do
      :ok
    else
      {:error, _reason} = error -> error
    end
  end

  defp validate_platform(platform) when is_map(platform) do
    expected = ["os", "arch"]

    if Enum.sort(Map.keys(platform)) == Enum.sort(expected) and
         valid_string?(platform["os"], 64) and
         valid_string?(platform["arch"], 64) do
      :ok
    else
      {:error, :invalid_platform}
    end
  end

  defp validate_platform(_), do: {:error, :invalid_platform}

  defp validate_sources(sources) do
    with :ok <- validate_all(sources, &validate_source/1),
         identities <- Enum.map(sources, &{&1["build"], &1["runtime"]}),
         true <-
           length(identities) == MapSet.size(MapSet.new(identities)) or
             {:error, :duplicate_source} do
      :ok
    else
      {:error, _reason} = error -> error
    end
  end

  defp validate_source(source) when is_map(source) do
    expected = ["build", "runtime", "upgrade_plan_sha256", "downgrade_plan_sha256"]

    if Enum.sort(Map.keys(source)) == Enum.sort(expected) and
         valid_string?(source["build"], 256) and
         valid_string?(source["runtime"], 256) and
         valid_hex?(source["upgrade_plan_sha256"], @sha256_hex_bytes) and
         valid_hex?(source["downgrade_plan_sha256"], @sha256_hex_bytes) do
      :ok
    else
      {:error, :invalid_source}
    end
  end

  defp validate_source(_), do: {:error, :invalid_source}

  defp validate_signature(%{"algorithm" => "ed25519", "value" => value} = signature)
       when map_size(signature) == 2 do
    if valid_hex?(value, 128), do: :ok, else: {:error, :invalid_signature}
  end

  defp validate_signature(_), do: {:error, :invalid_signature}

  defp verify_expected(unsigned, opts) do
    expected_publisher = Keyword.get(opts, :expected_publisher)
    expected_channel = Keyword.get(opts, :expected_channel)

    cond do
      not valid_hex?(expected_publisher, @public_key_hex_bytes) ->
        {:error, :invalid_expected_publisher}

      expected_channel not in @channels ->
        {:error, :invalid_expected_channel}

      unsigned["publisher"] != expected_publisher ->
        {:error, :publisher_mismatch}

      unsigned["channel"] != expected_channel ->
        {:error, :channel_mismatch}

      true ->
        :ok
    end
  end

  defp verify_expiry(unsigned, opts) do
    case Keyword.get(opts, :now) do
      now when is_integer(now) and now >= 0 ->
        if unsigned["expires_at"] > now, do: :ok, else: {:error, :expired}

      _ ->
        {:error, :invalid_now}
    end
  end

  defp verify_sequence(sequence, digest, opts) do
    case {Keyword.get(opts, :last_sequence), Keyword.get(opts, :last_digest)} do
      {nil, nil} ->
        :ok

      {last_sequence, last_digest}
      when is_integer(last_sequence) and last_sequence >= 0 and is_binary(last_digest) ->
        cond do
          sequence > last_sequence -> :ok
          sequence == last_sequence and last_digest == digest -> :ok
          sequence == last_sequence -> {:error, :sequence_digest_mismatch}
          true -> {:error, :replayed_sequence}
        end

      _ ->
        {:error, :invalid_sequence_state}
    end
  end

  defp installed_context(opts) do
    build = Keyword.get(opts, :installed_build)
    version = Keyword.get(opts, :installed_version)
    runtime = Keyword.get(opts, :installed_runtime)
    platform = Keyword.get(opts, :platform)
    pin = Keyword.get(opts, :pin)

    with true <- valid_string?(build, 256) or {:error, :invalid_installed_build},
         true <- valid_string?(runtime, 256) or {:error, :invalid_installed_runtime},
         {:ok, parsed_version} <- parse_version(version),
         :ok <- validate_platform(platform),
         true <- is_nil(pin) or valid_string?(pin, 256) or {:error, :invalid_pin} do
      {:ok, %{build: build, version: parsed_version, runtime: runtime, platform: platform}}
    else
      {:error, _reason} = error -> error
      :error -> {:error, :invalid_installed_version}
    end
  end

  defp eligible_edge(release, installed) do
    with true <- release["eligible"],
         false <- release["restart_required"],
         true <- release["runtime"] == installed.runtime,
         :gt <- Version.compare(Version.parse!(release["version"]), installed.version),
         source when not is_nil(source) <-
           Enum.find(
             release["sources"],
             &(&1["build"] == installed.build and &1["runtime"] == installed.runtime)
           ) do
      %{release: release, source: source}
    else
      _ -> nil
    end
  end

  defp selection_status(_latest, _installed, pin) when not is_nil(pin), do: :pinned
  defp selection_status(nil, _installed, _pin), do: :blocked

  defp selection_status(latest, installed, _pin) do
    case Version.compare(Version.parse!(latest["version"]), installed.version) do
      :eq -> :up_to_date
      :lt -> :blocked
      :gt -> if(latest["restart_required"], do: :restart_required, else: :blocked)
    end
  end

  defp install_status(nil, _candidate, _installed), do: {:blocked, :no_platform_release}

  defp install_status(latest, candidate, installed) do
    case Version.compare(Version.parse!(latest["version"]), installed) do
      :lt -> {:blocked, :lower_semver_target}
      :eq -> {:up_to_date, :current_release}
      :gt -> install_candidate_status(candidate, installed)
    end
  end

  defp install_candidate_status(nil, _installed), do: {:blocked, :no_install_archive}

  defp install_candidate_status(candidate, installed) do
    if Version.compare(Version.parse!(candidate["version"]), installed) == :gt,
      do: {:available, :newer_release},
      else: {:blocked, :no_install_archive}
  end

  defp parse_installed_version(value) do
    case parse_version(value) do
      {:ok, version} -> {:ok, version}
      :error -> {:error, :invalid_installed_version}
    end
  end

  defp selection_reason(_latest, _installed, pin) when not is_nil(pin), do: :pinned
  defp selection_reason(nil, _installed, _pin), do: :no_platform_release

  defp selection_reason(latest, installed, _pin) do
    case Version.compare(Version.parse!(latest["version"]), installed.version) do
      :eq -> :current_release
      :lt -> :lower_semver_target
      :gt -> if(latest["restart_required"], do: :restart_required, else: :no_compatible_hot_edge)
    end
  end

  defp parse_version(value) when is_binary(value) do
    case Version.parse(value) do
      {:ok, version} -> {:ok, version}
      :error -> :error
    end
  end

  defp parse_version(_), do: :error

  # Channel documents can list releases in any order. SemVer establishes which
  # advertised release is newer; exact source metadata still controls whether
  # the target may be applied to this running service.
  defp newest_first(releases) do
    Enum.sort(releases, fn left, right ->
      case Version.compare(Version.parse!(left["version"]), Version.parse!(right["version"])) do
        :gt -> true
        :lt -> false
        :eq -> left["build"] <= right["build"]
      end
    end)
  end

  defp signing_payload(unsigned),
    do: @domain <> Integer.to_string(unsigned["schema_version"]) <> "\0" <> canonical(unsigned)

  defp digest(unsigned) do
    unsigned
    |> canonical()
    |> then(&:crypto.hash(:sha256, &1))
    |> Base.encode16(case: :lower)
  end

  # This is deterministic JSON, not Erlang term serialization. Schema
  # validation above limits every value to JSON strings, integers, booleans,
  # arrays, and string-key maps; floats are intentionally never admitted.
  defp canonical(value) when is_map(value) do
    body =
      value
      |> Map.keys()
      |> Enum.sort()
      |> Enum.map(fn key -> [canonical_scalar(key), ?:, canonical(Map.fetch!(value, key))] end)
      |> Enum.intersperse(?,)

    IO.iodata_to_binary([?{, body, ?}])
  end

  defp canonical(value) when is_list(value) do
    value
    |> Enum.map(&canonical/1)
    |> Enum.intersperse(?,)
    |> then(&IO.iodata_to_binary([?[, &1, ?]]))
  end

  defp canonical(value), do: canonical_scalar(value)
  defp canonical_scalar(nil), do: "null"
  defp canonical_scalar(value), do: IO.iodata_to_binary(:json.encode(value))

  defp validate_all(items, validator) do
    Enum.reduce_while(items, :ok, fn item, :ok ->
      case validator.(item) do
        :ok -> {:cont, :ok}
        {:error, _reason} = error -> {:halt, error}
      end
    end)
  end

  defp decode_hex(value, bytes) when is_binary(value) and byte_size(value) == bytes * 2 do
    case Base.decode16(value, case: :lower) do
      {:ok, binary} when byte_size(binary) == bytes -> {:ok, binary}
      _ -> {:error, :invalid_hex}
    end
  end

  defp decode_hex(_, _), do: {:error, :invalid_hex}

  defp valid_hex?(value, expected_bytes)
       when is_binary(value) and byte_size(value) == expected_bytes do
    match?({:ok, _}, Base.decode16(value, case: :lower))
  end

  defp valid_hex?(_, _), do: false

  defp valid_string?(value, max_bytes) do
    is_binary(value) and value != "" and String.valid?(value) and byte_size(value) <= max_bytes
  end

  defp positive_integer?(value), do: is_integer(value) and value > 0
end
