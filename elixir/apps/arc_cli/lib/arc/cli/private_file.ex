defmodule Arc.CLI.PrivateFile do
  @moduledoc """
  Trusted client handling for immutable private files.

  Names and bytes are sealed locally to the active citizen. Signed headers
  bind the encrypted name and body hash; the resulting object ID is checked
  again on retrieval. Provider responses never choose a local output path.
  """

  alias Arc.Identity
  alias Arc.Identity.SealedBox

  @max_bytes 4 * 1024 * 1024
  @max_reply_bytes 6 * 1024 * 1024
  @prefix "sealed-v1:"

  def upload(path, %Identity{} = identity) when is_binary(path) and path != "" do
    with {:ok, %{type: :regular}} <- File.stat(path),
         {:ok, bytes} <- File.open(path, [:read, :binary], &IO.binread(&1, @max_bytes + 1)),
         bytes = if(bytes == :eof, do: "", else: bytes),
         true <- is_binary(bytes) and byte_size(bytes) <= @max_bytes,
         name = Path.basename(path),
         true <- String.valid?(name) and byte_size(name) <= 255 do
      file = seal(name, bytes, identity)
      {:ok, encode(%{"op" => "put", "file" => file})}
    else
      _ -> {:error, "cannot read file (regular files up to 4 MiB required)"}
    end
  end

  def upload(_, _), do: {:error, "a file path and active identity are required"}

  def request("get", values, input, output) do
    id = values[input["id"]]
    path = values[output["path"]]

    if valid_hex?(id, 64) and is_binary(path) and path != "" do
      {:ok, encode(%{"op" => "get", "id" => id})}
    else
      {:error, "a valid file ID and explicit --output path are required"}
    end
  end

  def request("list", values, input, _output) do
    cursor = values[input["after"]]

    if is_nil(cursor) or valid_hex?(cursor, 64) do
      request = if cursor, do: %{"op" => "list", "after" => cursor}, else: %{"op" => "list"}
      {:ok, encode(request)}
    else
      {:error, "invalid file cursor"}
    end
  end

  def request(_, _, _, _), do: {:error, "invalid private-file command"}

  def finish(text, built, output, %Identity{} = identity) do
    with {:ok, reply} <- decode(text),
         {:ok, request} <- decode(built.input) do
      finish_reply(output["operation"], reply, request, built.values, output, identity)
    end
  end

  def finish(_, _, _, _), do: {:error, "an active identity is required"}

  def seal(name, bytes, %Identity{} = identity) do
    {recipient, _} = Identity.to_x25519(identity)
    body = @prefix <> Base.encode64(SealedBox.seal(recipient, bytes))

    header = %{
      "version" => 1,
      "owner" => Identity.encode_public_key(identity),
      "name" => @prefix <> Base.encode64(SealedBox.seal(recipient, name)),
      "body_hash" => hash(body)
    }

    signature = Identity.sign(identity, signature_message(header)) |> Base.encode16(case: :lower)

    header
    |> Map.put("signature", signature)
    |> Map.put("id", hash(signature_message(header) <> "\n" <> signature))
    |> Map.put("body", body)
  end

  def verify_header(header, %Identity{} = identity) when is_map(header) do
    with %{
           "version" => 1,
           "owner" => owner,
           "name" => name,
           "body_hash" => body_hash,
           "signature" => signature,
           "id" => id
         } <- header,
         true <- owner == Identity.encode_public_key(identity),
         true <- is_binary(name) and byte_size(name) <= 2048,
         true <- valid_hex?(body_hash, 64) and valid_hex?(signature, 128) and valid_hex?(id, 64),
         {:ok, sig} <- Base.decode16(signature, case: :lower),
         true <- Identity.verify(identity.public_key, signature_message(header), sig),
         true <- id == hash(signature_message(header) <> "\n" <> signature),
         {:ok, filename} <- open(name, identity),
         true <- String.valid?(filename) and byte_size(filename) <= 255 do
      {:ok, filename}
    else
      _ -> {:error, "file header failed verification"}
    end
  end

  def verify_header(_, _), do: {:error, "file header failed verification"}

  defp finish_reply("put", %{"id" => id}, %{"op" => "put", "file" => file}, _, _, identity) do
    with {:ok, _name} <- verify_header(file, identity),
         true <- id == file["id"] do
      {:ok, id}
    else
      _ -> {:error, "provider did not acknowledge the uploaded file ID"}
    end
  end

  defp finish_reply(
         "get",
         %{"file" => file},
         %{"op" => "get", "id" => id},
         values,
         output,
         identity
       )
       when is_map(file) do
    with {:ok, _name} <- verify_header(file, identity),
         true <- file["id"] == id,
         body when is_binary(body) <- file["body"],
         true <- hash(body) == file["body_hash"],
         {:ok, bytes} <- open(body, identity),
         true <- byte_size(bytes) <= @max_bytes,
         :ok <- write_new(values[output["path"]], bytes) do
      {:ok, "Saved #{id} to #{values[output["path"]]}"}
    else
      {:error, message} -> {:error, message}
      _ -> {:error, "file contents failed verification"}
    end
  end

  defp finish_reply(
         "list",
         %{"files" => files, "next" => cursor},
         %{"op" => "list"},
         _,
         _,
         identity
       )
       when is_list(files) and length(files) <= 50 do
    cursor = if cursor == :null, do: nil, else: cursor

    with true <- is_nil(cursor) or valid_hex?(cursor, 64),
         {:ok, lines} <- list_lines(files, identity) do
      suffix = if cursor, do: "\nNext: --after #{cursor}", else: ""
      {:ok, Enum.join(lines, "\n") <> suffix}
    else
      _ -> {:error, "file listing failed verification"}
    end
  end

  defp finish_reply(_, _, _, _, _, _), do: {:error, "invalid private-file response"}

  defp list_lines(files, identity) do
    Enum.reduce_while(files, {:ok, []}, fn header, {:ok, lines} ->
      case verify_header(header, identity) do
        {:ok, name} -> {:cont, {:ok, lines ++ [header["id"] <> "\t" <> inspect(name)]}}
        error -> {:halt, error}
      end
    end)
  end

  defp open(@prefix <> encoded, identity) do
    with {:ok, sealed} <- Base.decode64(encoded),
         {:ok, bytes} <- SealedBox.open(identity, sealed) do
      {:ok, bytes}
    else
      _ -> {:error, "file decryption failed"}
    end
  rescue
    _ -> {:error, "file decryption failed"}
  end

  defp open(_, _), do: {:error, "file decryption failed"}

  defp write_new(path, bytes) when is_binary(path) and path != "" do
    case File.open(path, [:write, :binary, :exclusive]) do
      {:ok, device} ->
        result = with :ok <- File.chmod(path, 0o600), do: IO.binwrite(device, bytes)
        closed = File.close(device)

        if result == :ok and closed == :ok,
          do: :ok,
          else: {:error, "could not finish output file; inspect the requested destination"}

      {:error, :eexist} ->
        {:error, "output already exists; choose a new path"}

      {:error, _} ->
        {:error, "cannot create output file"}
    end
  end

  defp write_new(_, _), do: {:error, "an explicit --output path is required"}

  defp signature_message(header),
    do:
      "arc-private-file-v1\n" <>
        header["owner"] <> "\n" <> header["name"] <> "\n" <> header["body_hash"]

  defp hash(bytes), do: :crypto.hash(:sha256, bytes) |> Base.encode16(case: :lower)

  defp valid_hex?(value, length) when is_binary(value),
    do: byte_size(value) == length and Regex.match?(~r/\A[0-9a-f]+\z/, value)

  defp valid_hex?(_, _), do: false
  defp encode(value), do: value |> :json.encode() |> IO.iodata_to_binary()

  defp decode(text) when is_binary(text) and byte_size(text) <= @max_reply_bytes do
    case :json.decode(text) do
      value when is_map(value) -> {:ok, value}
      _ -> {:error, "invalid private-file response"}
    end
  rescue
    _ -> {:error, "invalid private-file response"}
  end

  defp decode(_), do: {:error, "invalid private-file response"}
end
