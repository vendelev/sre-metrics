#!/bin/bash
set -eu

# В официальном образе есть bash, но нет wget/curl.
exec 3<>/dev/tcp/127.0.0.1/13133
printf 'GET / HTTP/1.0\r\nHost: localhost\r\n\r\n' >&3
IFS= read -r -u 3 status
[[ "$status" == *" 200 "* ]]
