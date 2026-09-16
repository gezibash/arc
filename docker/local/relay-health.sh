#!/bin/sh
set -eu
test -s /home/arc/public/public_key
ERL_FLAGS='+S 1:1 +A 1' exec /app/bin/arc_runtime eval '
case :gen_tcp.connect(~c"127.0.0.1", 7331, [:binary, active: false], 2000) do
  {:ok, socket} -> :gen_tcp.close(socket)
  {:error, _} -> System.halt(1)
end
'
