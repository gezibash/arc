#!/usr/bin/env elixir

for line <- IO.stream(:stdio, :line) do
  event = :json.decode(line)

  unless event["message"] == "no-reply" do
    if event["message"] == "delayed", do: Process.sleep(500)

    reply = %{
      "message" => event["message"],
      "from" => event["from"],
      "method" => event["meta"]["method"],
      "path" => event["meta"]["path"]
    }

    IO.puts(:json.encode(%{"request_id" => event["request_id"], "reply" => reply}))
  end
end
