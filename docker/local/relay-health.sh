#!/bin/sh
set -eu
test -s /home/arc/public/public_key
exec timeout 2 bash -c 'exec 3<>/dev/tcp/127.0.0.1/7331'
