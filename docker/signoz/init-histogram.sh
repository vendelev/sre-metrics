#!/bin/sh
set -eu

# Версионированная UDF из официального развёртывания SigNoz, нужна для гистограмм.
target=/var/lib/clickhouse/user_scripts/histogramQuantile
if [ -x "$target" ]; then
    exit 0
fi

case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64) arch=arm64 ;;
    *) echo "Неподдерживаемая архитектура: $(uname -m)" >&2; exit 1 ;;
esac

archive="histogram-quantile_linux_${arch}.tar.gz"
base=https://github.com/SigNoz/signoz/releases/download/histogram-quantile/v0.0.1
cd /tmp
wget -q -O "$archive" "$base/$archive"
wget -q -O checksums.txt "$base/histogram-quantile_0.0.1_checksums.txt"
grep " $archive\$" checksums.txt | sha256sum -c -
tar -xzf "$archive" histogram-quantile
cp histogram-quantile "$target"
chmod 755 "$target"
