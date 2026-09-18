# Verify channel metadata and stage every archive through a pinned ARC relay.
# This is a publication proof, not an installer. The citizen identity is ephemeral.
{opts, rest, invalid} =
  OptionParser.parse(System.argv(),
    strict: [
      relay: :string,
      relay_pubkey: :string,
      provider: :string,
      publisher: :string,
      channel: :string,
      output: :string
    ]
  )

required = [:channel, :output, :provider, :publisher, :relay, :relay_pubkey]

unless rest == [] and invalid == [] and Enum.sort(Keyword.keys(opts)) == Enum.sort(required) do
  raise "required: --relay HOST:PORT --relay-pubkey HEX --provider HEX --publisher HEX --channel stable|beta --output NEW_DIRECTORY"
end

{:ok, %{relay: {host, port}, relay_pubkey: pin}} =
  Arc.CLI.RelaySettings.resolve(
    relay: opts[:relay],
    relay_pubkey: opts[:relay_pubkey]
  )

identity = Arc.Identity.generate()
{:ok, agent} = Arc.Data.Agent.start_link(identity)

try do
  :ok = Arc.Net.connect_relay(host, port, identity, pin)
  :ok = Arc.Data.Agent.publish_relay(agent)
  source = {:arc, agent, "releases+arc://#{opts[:provider]}/releases"}
  {:ok, bytes} = Arc.CLI.Update.Source.channel(source, opts[:channel])

  {:ok, verified} =
    Arc.CLI.Update.Manifest.verify(:json.decode(bytes),
      expected_publisher: opts[:publisher],
      expected_channel: opts[:channel],
      now: System.system_time(:second)
    )

  :ok = File.mkdir(opts[:output])

  for release <- verified.manifest["releases"] do
    archives = Enum.uniq([Map.take(release, ["sha256", "size"]) | List.wrap(release["install"])])

    for %{"sha256" => sha256, "size" => size} <- archives do
      path = Path.join(opts[:output], sha256 <> ".tar.gz")

      # A digest shared between releases is already staged and verified.
      unless File.exists?(path) do
        {:ok, ^path} = Arc.CLI.Update.Source.stage_archive(source, sha256, size, path)
      end
    end

    IO.puts(
      "Verified #{release["version"]} #{release["platform"]["os"]}/#{release["platform"]["arch"]}"
    )
  end

  IO.puts("Publisher signature and all archive digests verified through the relay.")
after
  GenServer.stop(agent, :normal)
end
