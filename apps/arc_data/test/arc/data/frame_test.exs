defmodule Arc.Data.FrameTest do
  use ExUnit.Case, async: true

  alias Arc.Data.Frame

  test "request frame roundtrip" do
    request_id = Frame.new_request_id()
    meta = %{"method" => "RAW", "path" => "/"}
    body = "hello"

    encoded = Frame.encode_request(request_id, meta, body)
    assert {:ok, decoded} = Frame.decode(encoded)

    assert decoded.version == 1
    assert decoded.type == :request
    assert decoded.request_id == request_id
    assert decoded.meta == meta
    assert decoded.body == body
  end

  test "response frame roundtrip" do
    request_id = Frame.new_request_id()
    meta = %{"status" => 200}
    body = "ok"

    encoded = Frame.encode_response(request_id, meta, body)
    assert {:ok, decoded} = Frame.decode(encoded)

    assert decoded.type == :response
    assert decoded.request_id == request_id
    assert decoded.meta == meta
    assert decoded.body == body
  end

  test "error frame roundtrip" do
    request_id = Frame.new_request_id()

    encoded = Frame.encode_error(request_id, "handler_error", "boom")
    assert {:ok, decoded} = Frame.decode(encoded)

    assert decoded.type == :error
    assert decoded.request_id == request_id
    assert decoded.meta["code"] == "handler_error"
    assert decoded.meta["message"] == "boom"
  end

  test "stream open frame roundtrip" do
    request_id = Frame.new_request_id()
    meta = %{"app_session_id" => "app-123", "method" => "RAW", "path" => "/shell"}
    body = "shell sb-1"

    encoded = Frame.encode_stream_open(request_id, meta, body)
    assert {:ok, decoded} = Frame.decode(encoded)

    assert decoded.type == :stream_open
    assert decoded.request_id == request_id
    assert decoded.meta == meta
    assert decoded.body == body
  end

  test "stream exit frame roundtrip" do
    request_id = Frame.new_request_id()
    meta = %{"app_session_id" => "app-123", "status" => 0}

    encoded = Frame.encode_stream_exit(request_id, meta, "bye")
    assert {:ok, decoded} = Frame.decode(encoded)

    assert decoded.type == :stream_exit
    assert decoded.request_id == request_id
    assert decoded.meta == meta
    assert decoded.body == "bye"
  end

  test "decode rejects unknown version" do
    request_id = Frame.new_request_id()
    encoded = Frame.encode_request(request_id, %{}, "x")
    <<_v, rest::binary>> = encoded
    unknown_version = <<2, rest::binary>>

    assert {:error, :unknown_version} = Frame.decode(unknown_version)
  end

  test "decode rejects unknown type" do
    request_id = Frame.new_request_id()
    encoded = Frame.encode_request(request_id, %{}, "x")
    <<version, _type, rest::binary>> = encoded
    unknown_type = <<version, 99, rest::binary>>

    assert {:error, :unknown_type} = Frame.decode(unknown_type)
  end

  test "decode rejects malformed payload lengths" do
    request_id = Frame.new_request_id()
    encoded = Frame.encode_request(request_id, %{}, "abc")

    <<version::8, type::8, flags::16, req::binary-size(16), _meta_len::32-big, rest::binary>> =
      encoded

    malformed =
      <<version::8, type::8, flags::16, req::binary-size(16), 10_000::32-big, rest::binary>>

    assert {:error, :malformed_frame} = Frame.decode(malformed)
  end
end
