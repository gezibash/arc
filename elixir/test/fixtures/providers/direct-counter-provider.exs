#!/usr/bin/env elixir

# A deterministic effect before a delayed reply, used to prove no replay or
# migration of a response after a direct lease expires.
for line <- IO.stream(:stdio, :line) do
  request = :json.decode(line)
  message = Base.decode64!(request["message"])

  if message == "commit" do
    File.write!(System.fetch_env!("ARC_TEST_DIRECT_RECEIPTS"), "committed\n", [:append])
    Process.sleep(700)
  end

  IO.puts(
    :json.encode(%{
      "op" => "reply",
      "request_id" => request["request_id"],
      "encoding" => "base64",
      "reply" => request["message"]
    })
  )
end
