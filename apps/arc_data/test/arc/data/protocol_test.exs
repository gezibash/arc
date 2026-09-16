defmodule Arc.Data.ProtocolTest do
  use ExUnit.Case, async: false

  alias Arc.Data.Agent
  alias Arc.Data.Protocol
  alias Arc.Identity

  @fixtures Path.expand("../../../../../test/fixtures/providers", __DIR__)

  setup do
    Arc.Control.Local.reset()
    root = Path.join(System.tmp_dir!(), "arc-protocol-#{System.unique_integer([:positive])}")
    File.mkdir_p!(root)
    on_exit(fn -> File.rm_rf!(root) end)
    %{root: root}
  end

  test "addresses require full keys and unambiguous resource paths" do
    key = String.duplicate("ab", 32)
    assert {:ok, target} = Protocol.parse("sqlite+arc://#{String.upcase(key)}/main")
    assert target.provider == key
    assert target.scheme == "sqlite"
    assert target.path == "/main"

    for address <- [
          "sqlite://#{key}/main",
          "sqlite+arc://friendly-name/main",
          "sqlite+arc://user@#{key}/main",
          "sqlite+arc://#{key}:80/main",
          "sqlite+arc://#{key}/main?file=other",
          "sqlite+arc://#{key}/main#other",
          "sqlite+arc://#{key}/../private",
          "sqlite+arc://#{key}/%2e%2e/private",
          "sqlite+arc://#{key}/info/capabilities/primary",
          "sqlite+arc://#{key}/main\n",
          String.duplicate("x", 4_097)
        ] do
      assert {:error, :invalid_protocol_uri} = Protocol.parse(address)
    end
  end

  test "signed capability selects method while URI selects the resource", ctx do
    {client, provider, _server} = start_pair(ctx.root)
    assert {:ok, %{body: body}} = Protocol.request(client, uri(provider), "hello 🌍\n")
    result = :json.decode(body)
    assert result["message"] == "hello 🌍\n"
    assert result["method"] == "QUERY"
    assert result["path"] == "/main"
    assert result["from"] == Base.encode16(Agent.info(client).public_key, case: :lower)
  end

  test "mismatched scheme and stream profile fail before application calls", ctx do
    {client, provider, _server} = start_pair(ctx.root)
    assert {:error, :scheme_mismatch} = Protocol.request(client, uri(provider, "wrong"), "hello")

    assert {:error, {:remote, _, _}} =
             Protocol.request(client, uri(provider), "hello", capability: "absent")

    {stream_client, stream_provider, _server} = start_pair(ctx.root, mode: "stream")

    assert {:error, :unsupported_transport_profile} =
             Protocol.request(stream_client, uri(stream_provider), "hello")
  end

  test "binary body requires explicit provider support", ctx do
    {client, provider, _server} = start_pair(ctx.root)

    assert {:error, :binary_encoding_required} =
             Protocol.request(client, uri(provider), <<0, 255>>)
  end

  test "malformed signed body descriptions fail before application submission", ctx do
    {client, provider, _server} = start_pair(ctx.root, response_body: "invalid")
    assert {:error, :invalid_body_contract} = Protocol.request(client, uri(provider), "hello")
  end

  test "opaque bytes survive generic URI requests with binary encoding", ctx do
    {client, provider, _server} = start_pair(ctx.root, binary: true)
    body = :binary.copy(for(n <- 0..255, into: <<>>, do: <<n>>), 512)

    assert {:ok, %{body: ^body}} =
             Protocol.request(client, uri(provider, "binary-echo"), body)
  end

  test "invalid limits are rejected without attempting discovery" do
    address = "echo+arc://#{String.duplicate("a", 64)}/"
    oversized = :binary.copy(<<0>>, Protocol.max_body_bytes() + 1)
    assert {:error, :request_too_large} = Protocol.request(self(), address, oversized)
    assert {:error, :invalid_timeout} = Protocol.request(self(), address, "", timeout_ms: 0)

    assert {:error, :invalid_capability_id} =
             Protocol.request(self(), address, "", capability: "../")
  end

  test "replies from another citizen cannot satisfy a matching request", ctx do
    {client, provider, server} = start_pair(ctx.root)
    task = Task.async(fn -> Protocol.request(client, uri(provider), "delayed") end)
    request_id = pending_request_id(server, 100)
    fake_key = Identity.generate().public_key

    fake = %{
      from_key: fake_key,
      request_id: request_id,
      kind: :response,
      text: "forged",
      meta: %{}
    }

    :sys.replace_state(client, fn state -> %{state | inbox: [fake | state.inbox]} end)
    assert {:ok, %{body: body}} = Task.await(task)
    assert :json.decode(body)["message"] == "delayed"
    assert [^fake] = Agent.take_inbox(client, &(&1[:from_key] == fake_key))
  end

  test "an unanswered submitted request has an unknown outcome and is not retried", ctx do
    {client, provider, server} = start_pair(ctx.root)

    assert {:error, :outcome_unknown} =
             Protocol.request(client, uri(provider), "no-reply", timeout_ms: 500)

    {_module, state} = :sys.get_state(server).handler
    assert map_size(state.pending_requests) == 1
  end

  test "losing the caller after submission preserves the unknown outcome", ctx do
    {client, provider, server} = start_pair(ctx.root)
    pending = Task.async(fn -> Protocol.request(client, uri(provider), "no-reply") end)
    _request_id = pending_request_id(server, 100)
    GenServer.stop(client, :normal)
    assert {:error, :outcome_unknown} = Task.await(pending)
    {_module, state} = :sys.get_state(server).handler
    assert map_size(state.pending_requests) == 1
  end

  defp start_pair(root, opts \\ []) do
    client_id = Identity.generate()
    provider_id = Identity.generate()
    {:ok, client} = Agent.start_link(client_id)

    {runtime, manifest} =
      if opts[:binary] do
        {Path.join(@fixtures, "binary-echo-provider.exs"),
         Path.join(@fixtures, "binary-echo-provider.json")}
      else
        manifest = Path.join(root, "manifest-#{System.unique_integer([:positive])}.json")

        File.write!(
          manifest,
          :json.encode(%{
            "capability" => %{
              "id" => "primary",
              "scheme" => "echo",
              "kind" => "data",
              "title" => "Echo",
              "summary" => "Protocol test",
              "invocation" => %{
                "mode" => opts[:mode] || "request_reply",
                "method" => "QUERY",
                "path" => "/default",
                "response_body" => opts[:response_body] || %{"type" => "text"}
              }
            }
          })
        )

        {Path.join(@fixtures, "protocol-echo-provider.exs"), manifest}
      end

    File.chmod!(runtime, 0o755)

    {:ok, server} =
      Agent.start_link(provider_id,
        serve: "exec://#{runtime}?#{URI.encode_query(%{"manifest" => manifest})}"
      )

    :ok = Agent.publish(client)
    :ok = Agent.publish(server)

    on_exit(fn ->
      for process <- [client, server],
          Process.alive?(process),
          do: GenServer.stop(process, :normal)
    end)

    {client, provider_id, server}
  end

  defp uri(provider, scheme \\ "echo"),
    do: "#{scheme}+arc://#{Identity.encode_public_key(provider)}/main"

  defp pending_request_id(_server, 0), do: flunk("application request was not received")

  defp pending_request_id(server, attempts) do
    {_module, state} = :sys.get_state(server).handler

    case Map.values(state.pending_requests) do
      [%{request_id: request_id}] ->
        request_id

      [] ->
        Process.sleep(10)
        pending_request_id(server, attempts - 1)
    end
  end
end
