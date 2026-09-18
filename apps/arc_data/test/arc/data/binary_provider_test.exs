defmodule Arc.Data.BinaryProviderTest do
  use ExUnit.Case, async: false

  alias Arc.Data.Agent
  alias Arc.Data.Handler.Exec
  alias Arc.Identity

  @from_pk <<17::256>>
  @max_binary_body_bytes 1_024 * 1_024

  test "base64 opt-in preserves opaque bytes through an exec provider" do
    {:ok, state} = binary_provider_state()
    body = :binary.list_to_bin(Enum.to_list(0..255)) <> <<0, 255, 128, 10, 13>>
    request_id = <<1::128>>

    {:noreply, state} =
      Exec.handle_message(body, @from_pk, %{request_id: request_id, framed?: true}, state)

    assert {:emit, [event], _state} = drain_until_event(state, 30_000)
    assert event.frame_type == :response
    assert event.request_id == request_id
    assert event.body == body
  end

  test "base64 replies larger than a port read chunk are reassembled and decoded" do
    {:ok, state} = binary_provider_state()
    body = :binary.copy(<<0, 255, 128>>, 30_000)

    {:noreply, state} =
      Exec.handle_message(body, @from_pk, %{request_id: <<2::128>>, framed?: true}, state)

    assert {:emit, [event], _state} = drain_until_event(state, 30_000)
    assert event.body == body
  end

  test "rejects a binary request above one mebibyte before it reaches the provider" do
    {:ok, state} = binary_provider_state()
    body = :binary.copy(<<0>>, @max_binary_body_bytes + 1)
    request_id = <<3::128>>

    assert {:emit, [event], state} =
             Exec.handle_message(body, @from_pk, %{request_id: request_id, framed?: true}, state)

    assert event.frame_type == :error
    assert event.request_id == request_id
    assert event.meta["message"] =~ "exceeds"
    assert state.pending_requests == %{}
    port = state.port
    refute_receive {^port, {:data, _}}, 100
  end

  test "uses the manifest request body cap before serializing the provider request" do
    {:ok, state} = binary_provider_state(max_bytes: 4)
    request_id = <<14::128>>

    assert state.max_request_bytes == 4

    assert {:emit, [event], state} =
             Exec.handle_message(
               <<0, 1, 2, 3, 4>>,
               @from_pk,
               %{request_id: request_id, framed?: true},
               state
             )

    assert event.frame_type == :error
    assert event.request_id == request_id
    assert event.meta["message"] =~ "exceeds 4 bytes"
    assert state.pending_requests == %{}
    port = state.port
    refute_receive {^port, {:data, _}}, 100
  end

  test "rejects invalid manifest request body caps at provider startup" do
    for max_bytes <- [0, @max_binary_body_bytes * 65] do
      manifest = temporary_manifest(max_bytes)
      runtime = fixture_path("binary-echo-provider.exs")

      assert {:error, {:invalid_request_body_max_bytes, ^max_bytes}} =
               Exec.init("exec://#{runtime}?manifest=#{URI.encode_www_form(manifest)}")
    end
  end

  test "rejects malformed request body descriptions without crashing startup" do
    manifest = temporary_manifest(4)
    document = manifest |> File.read!() |> :json.decode()
    document = put_in(document, ["capability", "invocation", "request_body"], "invalid")
    File.write!(manifest, :json.encode(document))
    runtime = fixture_path("binary-echo-provider.exs")

    assert {:error, :invalid_request_body} =
             Exec.init("exec://#{runtime}?manifest=#{URI.encode_www_form(manifest)}")
  end

  test "direct agent messages cannot bypass the provider request body cap" do
    manifest = temporary_manifest(4)
    runtime = fixture_path("binary-echo-provider.exs")
    File.chmod!(runtime, 0o755)
    client_identity = Identity.generate()
    provider_identity = Identity.generate()

    {:ok, client} = Agent.start_link(client_identity)

    {:ok, provider} =
      Agent.start_link(provider_identity,
        serve: "exec://#{runtime}?manifest=#{URI.encode_www_form(manifest)}"
      )

    on_exit(fn ->
      Enum.each([client, provider], &Arc.Data.TestTeardown.stop/1)
    end)

    :ok = Agent.publish(client)
    :ok = Agent.publish(provider)
    provider_key = Identity.encode_public_key(provider_identity.public_key)
    assert {:ok, _entry} = Agent.connect(client, provider_key)

    request_id = <<16::128>>

    assert :ok =
             Agent.send_message(client, provider_key, <<0, 1, 2, 3, 4>>,
               request_id: request_id,
               meta: %{"method" => "RAW"}
             )

    assert %{kind: :error, request_id: ^request_id, meta: meta} =
             await_inbox_message(client, request_id, 100)

    assert meta["message"] =~ "exceeds 4 bytes"

    {_module, handler_state} = :sys.get_state(provider).handler
    assert handler_state.pending_requests == %{}
  end

  test "rejects malformed, missing, and unexpected encodings as correlated errors" do
    for {request_id, event, expected_message} <- [
          {<<4::128>>, %{"encoding" => "base64", "reply" => "%%%"}, "not valid base64"},
          {<<5::128>>, %{"reply" => Base.encode64("body")}, "missing base64 encoding"},
          {<<6::128>>, %{"encoding" => "hex", "reply" => "626f6479"}, "unexpected encoding"}
        ] do
      {:ok, state} = binary_provider_state()

      {:noreply, state} =
        Exec.handle_message("request", @from_pk, %{request_id: request_id, framed?: true}, state)

      line =
        event
        |> Map.put("op", "reply")
        |> Map.put("request_id", Base.encode16(request_id, case: :lower))
        |> :json.encode()
        |> IO.iodata_to_binary()

      assert {:emit, [reply], state} =
               Exec.handle_info({state.port, {:data, {:eol, line}}}, state)

      assert reply.frame_type == :error
      assert reply.request_id == request_id
      assert reply.meta["message"] =~ expected_message
      assert state.pending_requests == %{}
    end
  end

  test "rejects a decoded binary reply above one mebibyte" do
    {:ok, state} = binary_provider_state()
    request_id = <<7::128>>

    {:noreply, state} =
      Exec.handle_message("request", @from_pk, %{request_id: request_id, framed?: true}, state)

    line =
      :json.encode(%{
        "op" => "reply",
        "request_id" => Base.encode16(request_id, case: :lower),
        "encoding" => "base64",
        "reply" => Base.encode64(:binary.copy(<<0>>, @max_binary_body_bytes + 1))
      })
      |> IO.iodata_to_binary()

    assert {:emit, [event], state} = Exec.handle_info({state.port, {:data, {:eol, line}}}, state)
    assert event.frame_type == :error
    assert event.request_id == request_id
    assert event.meta["message"] =~ "exceeds"
    assert state.pending_requests == %{}
  end

  test "a malformed reply fails its matching request without consuming an older request" do
    {:ok, state} = binary_provider_state()
    first_id = <<11::128>>
    second_id = <<12::128>>

    {:noreply, state} =
      Exec.handle_message("first", @from_pk, %{request_id: first_id, framed?: true}, state)

    {:noreply, state} =
      Exec.handle_message("second", @from_pk, %{request_id: second_id, framed?: true}, state)

    line =
      :json.encode(%{
        "op" => "reply",
        "request_id" => Base.encode16(second_id, case: :lower),
        "encoding" => "base64",
        "reply" => "%%%"
      })
      |> IO.iodata_to_binary()

    assert {:emit, [event], state} = Exec.handle_info({state.port, {:data, {:eol, line}}}, state)
    assert event.frame_type == :error
    assert event.request_id == second_id
    assert Map.has_key?(state.pending_requests, Base.encode16(first_id, case: :lower))
    refute Map.has_key?(state.pending_requests, Base.encode16(second_id, case: :lower))
  end

  test "a binary reply without a request id becomes a correlated error" do
    {:ok, state} = binary_provider_state()
    request_id = <<13::128>>

    {:noreply, state} =
      Exec.handle_message("request", @from_pk, %{request_id: request_id, framed?: true}, state)

    line =
      :json.encode(%{"encoding" => "base64", "reply" => Base.encode64("body")})
      |> IO.iodata_to_binary()

    assert {:emit, [event], _state} = Exec.handle_info({state.port, {:data, {:eol, line}}}, state)
    assert event.frame_type == :error
    assert event.request_id == request_id
    assert event.meta["message"] =~ "missing request_id"
  end

  test "legacy text providers retain their plain reply contract" do
    {:ok, state} = hello_provider_state()
    request_id = <<8::128>>

    {:noreply, state} =
      Exec.handle_message(
        "POST /echo still text",
        @from_pk,
        %{request_id: request_id, framed?: true},
        state
      )

    assert {:emit, [event], _state} = drain_until_event(state, 30_000)
    assert event.frame_type == :response
    assert event.body == "still text"
  end

  test "a provider exit fails pending binary requests with their request ids" do
    {:ok, state} = binary_provider_state()
    first_id = <<9::128>>
    second_id = <<10::128>>

    {:noreply, state} =
      Exec.handle_message("one", @from_pk, %{request_id: first_id, framed?: true}, state)

    {:noreply, state} =
      Exec.handle_message("two", @from_pk, %{request_id: second_id, framed?: true}, state)

    assert {:emit, events, state} = Exec.handle_info({state.port, {:exit_status, 17}}, state)
    assert Enum.map(events, & &1.request_id) == [first_id, second_id]
    assert Enum.all?(events, &(&1.frame_type == :error))
    assert state.pending_requests == %{}
  end

  test "rejects a repeated pending request id without replacing or forwarding the original" do
    {:ok, state} = binary_provider_state()
    request_id = <<15::128>>
    original_recipient = @from_pk
    duplicate_recipient = <<18::256>>

    {:noreply, state} =
      Exec.handle_message(
        "first",
        original_recipient,
        %{request_id: request_id, framed?: true},
        state
      )

    port = state.port

    assert_receive {^port, {:data, _}}, 30_000

    assert {:emit, [event], state} =
             Exec.handle_message(
               "duplicate",
               duplicate_recipient,
               %{request_id: request_id, framed?: true},
               state
             )

    correlation_id = Base.encode16(request_id, case: :lower)
    assert event.frame_type == :error
    assert event.request_id == request_id
    assert event.to_pk == duplicate_recipient
    assert event.meta["message"] =~ "already pending"
    assert state.pending_order == [correlation_id]
    assert get_in(state.pending_requests, [correlation_id, :to_pk]) == original_recipient
    refute_receive {^port, {:data, _}}, 100
  end

  test "rejects an overloaded provider queue without forwarding the extra request" do
    {:ok, state} = binary_provider_state()

    pending_requests =
      Map.new(1..256, fn index ->
        {"pending-#{index}",
         %{to_pk: @from_pk, request_id: <<index::128>>, framed?: true, binary_response?: true}}
      end)

    state = %{
      state
      | pending_requests: pending_requests,
        pending_order: Map.keys(pending_requests)
    }

    request_id = <<257::128>>

    assert {:emit, [event], state} =
             Exec.handle_message(
               "one more",
               @from_pk,
               %{request_id: request_id, framed?: true},
               state
             )

    assert event.frame_type == :error
    assert event.request_id == request_id
    assert event.meta["message"] =~ "queue is full"
    assert map_size(state.pending_requests) == 256
    port = state.port
    refute_receive {^port, {:data, _}}, 100
  end

  defp binary_provider_state(opts \\ []) do
    manifest =
      case Keyword.fetch(opts, :max_bytes) do
        {:ok, max_bytes} -> temporary_manifest(max_bytes)
        :error -> fixture_path("binary-echo-provider.json")
      end

    provider_state("binary-echo-provider.exs", manifest)
  end

  defp hello_provider_state do
    provider_state("hello-provider.exs", fixture_path("hello-provider.json"))
  end

  defp provider_state(runtime_name, manifest_path) do
    runtime = fixture_path(runtime_name)
    File.chmod!(runtime, 0o755)
    Exec.init("exec://#{runtime}?manifest=#{URI.encode_www_form(manifest_path)}")
  end

  defp fixture_path(name),
    do: Path.expand("../../../../../test/fixtures/providers/#{name}", __DIR__)

  defp temporary_manifest(max_bytes) do
    path =
      Path.join(
        System.tmp_dir!(),
        "arc-binary-provider-#{System.unique_integer([:positive])}.json"
      )

    File.write!(
      path,
      :json.encode(%{
        "published_at" => "2026-09-16T00:00:00Z",
        "release" => %{"version" => "1.0.0", "channel" => "stable"},
        "capability" => %{
          "id" => "primary",
          "kind" => "data",
          "scheme" => "binary-echo",
          "title" => "Binary Echo",
          "summary" => "Test provider",
          "invocation" => %{
            "method" => "RAW",
            "path" => "/",
            "request_body" => %{
              "type" => "bytes",
              "encoding" => "base64",
              "max_bytes" => max_bytes
            },
            "response_body" => %{"type" => "bytes", "encoding" => "base64"}
          }
        }
      })
    )

    on_exit(fn -> File.rm(path) end)
    path
  end

  defp await_inbox_message(_agent, _request_id, 0), do: flunk("expected ARC error reply")

  defp await_inbox_message(agent, request_id, attempts) do
    case Agent.take_inbox(agent, &(&1[:request_id] == request_id)) do
      [message] ->
        message

      [] ->
        Process.sleep(10)
        await_inbox_message(agent, request_id, attempts - 1)
    end
  end

  defp drain_until_event(state, timeout) do
    deadline = System.monotonic_time(:millisecond) + timeout

    receive do
      {port, {:data, _}} = message when port == state.port ->
        case Exec.handle_info(message, state) do
          {:noreply, state} ->
            drain_until_event(state, deadline - System.monotonic_time(:millisecond))

          result ->
            result
        end
    after
      max(timeout, 0) -> flunk("expected provider reply before timeout")
    end
  end
end
