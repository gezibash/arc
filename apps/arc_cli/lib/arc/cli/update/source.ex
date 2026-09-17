defmodule Arc.CLI.Update.Source do
  @moduledoc """
  Fetch bounded release metadata and immutable archives from ARC or an explicitly
  selected local release directory.

  An ARC source is `{:arc, agent, "releases+arc://<provider-key>/releases"}`.
  A local source is `{:local, absolute_root}` and is never selected implicitly.
  This module verifies transport size and archive bytes. Publisher signatures,
  channel policy, and release installation belong to separate layers.
  """

  alias Arc.Data.Protocol

  @max_channel_bytes 256 * 1024
  @max_chunk_bytes 256 * 1024
  @max_chunk_reply_bytes 512 * 1024
  @default_deadline_ms 120_000
  @hex ~r/\A[0-9a-f]{64}\z/

  @type source :: {:arc, pid(), String.t()} | {:local, String.t()}

  @spec channel(source(), String.t(), keyword()) :: {:ok, binary()} | {:error, term()}
  def channel(source, channel, opts \\ [])

  def channel(source, channel, opts) when channel in ["stable", "beta"] do
    with {:ok, deadline} <- deadline(opts) do
      fetch_channel(source, channel, deadline, opts)
    end
  end

  def channel(_, _, _), do: {:error, :invalid_channel}

  @doc """
  Stream an immutable archive into a new destination. `sha256` is plain lower
  case hexadecimal; the provider wire format uses `sha256:<hex>`.
  """
  @spec stage_archive(source(), String.t(), non_neg_integer(), String.t(), keyword()) ::
          {:ok, String.t()} | {:error, term()}
  def stage_archive(source, sha256, size, destination, opts \\ [])

  def stage_archive(source, sha256, size, destination, opts)
      when is_binary(sha256) and is_integer(size) and size >= 0 and is_binary(destination) do
    with true <- Regex.match?(@hex, sha256) or {:error, :invalid_digest},
         {:ok, max_bytes} <- max_bytes(opts),
         true <- size <= max_bytes or {:error, :archive_too_large},
         {:ok, deadline} <- deadline(opts),
         :ok <- destination_absent?(destination),
         {:ok, temp} <- open_temp(destination) do
      write_archive(source, sha256, size, temp, destination, deadline, opts)
    else
      {:error, _} = error -> error
    end
  end

  def stage_archive(_, _, _, _, _), do: {:error, :invalid_request}

  # Short aliases keep the update coordinator readable.
  def stage(source, sha256, size, destination, opts \\ []),
    do: stage_archive(source, sha256, size, destination, opts)

  defp fetch_channel({:arc, agent, address}, channel, deadline, opts)
       when is_pid(agent) and is_binary(address) do
    request(
      agent,
      address,
      %{"op" => "channel", "channel" => channel},
      deadline,
      @max_channel_bytes,
      opts
    )
  end

  defp fetch_channel({:local, root}, channel, _deadline, _opts) when is_binary(root) do
    with :ok <- safe_directory(root),
         :ok <- safe_directory(Path.join(root, "channels")) do
      root
      |> channel_path(channel)
      |> read_regular(@max_channel_bytes)
    end
  end

  defp fetch_channel(_, _, _, _), do: {:error, :invalid_source}

  defp write_archive(source, digest, size, {temp_path, io}, destination, deadline, opts) do
    result =
      try do
        with {:ok, hash} <-
               :crypto.hash_init(:sha256)
               |> write_chunks(source, digest, size, 0, io, deadline, opts),
             :ok <- :file.sync(io) do
          {:ok, hash}
        else
          {:error, _} = error -> error
        end
      after
        File.close(io)
      end

    case result do
      {:ok, hash} ->
        finish_archive({:ok, hash}, size, temp_path, destination, digest)

      {:error, _} = error ->
        File.rm(temp_path)
        error
    end
  end

  defp write_chunks(hash, _source, _digest, size, size, _io, _deadline, _opts), do: {:ok, hash}

  defp write_chunks(hash, source, digest, size, offset, io, deadline, opts) do
    length = min(@max_chunk_bytes, size - offset)

    with {:ok, chunk} <- fetch_chunk(source, digest, offset, length, deadline, opts),
         true <- (byte_size(chunk) > 0 and byte_size(chunk) <= length) or {:error, :invalid_chunk},
         :ok <- IO.binwrite(io, chunk) do
      hash
      |> :crypto.hash_update(chunk)
      |> write_chunks(source, digest, size, offset + byte_size(chunk), io, deadline, opts)
    else
      {:error, _} = error -> error
    end
  end

  defp finish_archive({:ok, hash}, _size, temp_path, destination, digest) do
    actual = hash |> :crypto.hash_final() |> Base.encode16(case: :lower)

    with true <- actual == digest or {:error, :digest_mismatch},
         :ok <- link_new(temp_path, destination) do
      :ok = File.rm(temp_path)
      {:ok, destination}
    else
      {:error, _} = error ->
        case File.rm(temp_path) do
          :ok -> error
          {:error, reason} -> {:error, {:staging_cleanup_failed, reason}}
        end
    end
  end

  defp fetch_chunk({:arc, agent, address}, digest, offset, length, deadline, opts)
       when is_pid(agent) and is_binary(address) do
    with {:ok, body} <-
           request(
             agent,
             address,
             %{
               "op" => "chunk",
               "digest" => "sha256:" <> digest,
               "offset" => offset,
               "length" => length
             },
             deadline,
             @max_chunk_reply_bytes,
             opts
           ),
         {:ok, response} <- decode_json(body),
         ^offset <- response["offset"],
         "sha256:" <> ^digest <- response["digest"],
         data when is_binary(data) <- response["data"],
         {:ok, bytes} <- Base.decode64(data) do
      {:ok, bytes}
    else
      _ -> {:error, :invalid_chunk}
    end
  end

  defp fetch_chunk({:local, root}, digest, offset, length, _deadline, _opts)
       when is_binary(root) do
    with {:ok, path} <- blob_path(root, digest),
         {:ok, %{size: size}} <- regular_stat(path),
         true <- offset <= size or {:error, :invalid_chunk},
         {:ok, io} <- File.open(path, [:read, :binary]) do
      try do
        with {:ok, _} <- :file.position(io, offset),
             bytes when is_binary(bytes) <- IO.binread(io, min(length, size - offset)) do
          {:ok, bytes}
        else
          _ -> {:error, :source_read_failed}
        end
      after
        File.close(io)
      end
    else
      {:error, :enoent} -> {:error, :not_found}
      {:error, _} -> {:error, :source_read_failed}
    end
  end

  defp fetch_chunk(_, _, _, _, _, _), do: {:error, :invalid_source}

  defp request(agent, address, request, deadline, limit, opts) do
    remaining = deadline - System.monotonic_time(:millisecond)
    timeout = Keyword.get(opts, :request_timeout_ms, 30_000)

    cond do
      remaining <= 0 ->
        {:error, :deadline_exceeded}

      not (is_integer(timeout) and timeout > 0 and timeout <= 120_000) ->
        {:error, :invalid_request_timeout}

      true ->
        with {:ok, response} <-
               Protocol.request(agent, address, encode(request),
                 capability: Keyword.get(opts, :capability, "primary"),
                 timeout_ms: min(remaining, timeout)
               ),
             true <- byte_size(response.body) <= limit or {:error, :response_too_large} do
          {:ok, response.body}
        else
          {:error, _} = error -> error
        end
    end
  end

  defp deadline(opts) do
    timeout = Keyword.get(opts, :deadline_ms, @default_deadline_ms)

    if is_integer(timeout) and timeout > 0 and timeout <= 120_000,
      do: {:ok, System.monotonic_time(:millisecond) + timeout},
      else: {:error, :invalid_deadline}
  end

  defp max_bytes(opts) do
    value = Keyword.get(opts, :max_bytes, 2 * 1024 * 1024 * 1024)
    if is_integer(value) and value >= 0, do: {:ok, value}, else: {:error, :invalid_max_bytes}
  end

  defp destination_absent?(destination) do
    case File.lstat(Path.dirname(destination)) do
      {:ok, %{type: :directory}} ->
        case File.lstat(destination) do
          {:error, :enoent} -> :ok
          _ -> {:error, :destination_exists}
        end

      _ ->
        {:error, :invalid_destination}
    end
  end

  defp open_temp(destination) do
    suffix = Base.url_encode64(:crypto.strong_rand_bytes(16), padding: false)
    path = Path.join(Path.dirname(destination), ".#{Path.basename(destination)}.#{suffix}.part")

    case File.open(path, [:write, :binary, :exclusive]) do
      {:ok, io} -> {:ok, {path, io}}
      _ -> {:error, :staging_open_failed}
    end
  end

  # A hard link creates the final name atomically and refuses an existing
  # destination. It is safer than File.rename/2, whose overwrite behavior is
  # platform-dependent. The staging file and destination share a directory.
  defp link_new(temp, destination) do
    case File.ln(temp, destination) do
      :ok -> :ok
      {:error, :eexist} -> {:error, :destination_exists}
      _ -> {:error, :staging_commit_failed}
    end
  end

  defp channel_path(root, channel), do: Path.join([root, "channels", channel <> ".json"])

  defp blob_path(root, digest) do
    with :ok <- safe_directory(root),
         :ok <- safe_directory(Path.join(root, "blobs")) do
      {:ok, Path.join([root, "blobs", digest <> ".tar.gz"])}
    end
  end

  defp read_regular(path, limit) do
    with {:ok, %{size: size}} <- regular_stat(path),
         true <- size <= limit or {:error, :response_too_large},
         {:ok, bytes} <- File.read(path) do
      {:ok, bytes}
    else
      {:error, :enoent} -> {:error, :not_found}
      _ -> {:error, :source_read_failed}
    end
  end

  defp decode_json(body) do
    {:ok, :json.decode(body)}
  rescue
    _ -> {:error, :invalid_chunk}
  end

  defp regular_stat(path) do
    case File.lstat(path) do
      {:ok, %{type: :regular} = stat} -> {:ok, stat}
      {:ok, _} -> {:error, :unsafe_file}
      {:error, _} = error -> error
    end
  end

  defp safe_directory(path) do
    case File.lstat(path) do
      {:ok, %{type: :directory}} -> :ok
      {:error, :enoent} -> {:error, :not_found}
      _ -> {:error, :source_read_failed}
    end
  end

  defp encode(map), do: map |> :json.encode() |> IO.iodata_to_binary()
end
