# The publisher key remains in the operator keystore, never in provider storage.
# Input is a complete unsigned manifest; hashes and byte lengths are verified
# against existing blobs before the signed channel is atomically replaced.
{opts, rest, invalid} =
  OptionParser.parse(System.argv(),
    strict: [root: :string, manifest: :string, key: :string]
  )

unless rest == [] and invalid == [] and Enum.sort(Keyword.keys(opts)) == [:key, :manifest, :root] do
  raise "usage: mix run scripts/publish-release-channel.exs --root PATH --manifest PATH --key NAME"
end

result =
  with {:ok, identity} <- Arc.Identity.KeyStore.get(opts[:key]),
       {:ok, %{type: :regular, size: size}} when size <= 262_144 <- File.lstat(opts[:manifest]),
       {:ok, bytes} <- File.read(opts[:manifest]) do
    document = :json.decode(bytes)
    Arc.CLI.Update.Publisher.publish(Path.expand(opts[:root]), identity, document)
  end

case result do
  :ok -> IO.puts("Signed release channel published.")
  {:error, reason} when is_atom(reason) -> raise "channel publication refused: #{reason}"
  _ -> raise "channel publication refused"
end
