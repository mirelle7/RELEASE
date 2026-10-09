#!/bin/sh
# Build the agent. Embeds the current ../site into the program, then compiles it for Windows (and this machine).
# Needs only Go (https://go.dev/dl). Output goes to ../site/downloads/ so the website can offer it.
# Builds: Windows x64 and x86, macOS on Apple silicon, Linux x64 and Linux ARM64.
set -eu
cd "$(dirname "$0")"
rm -rf site && mkdir site
for f in ../site/*; do
  case "$(basename "$f")" in downloads|builds) continue ;; esac
  cp -R "$f" site/
done
touch site/.gitkeep
mkdir -p ../site/downloads
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../site/downloads/build-agent-windows-x64.exe .
GOOS=windows GOARCH=386   CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../site/downloads/build-agent-windows-x86.exe .
GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../site/downloads/build-agent-macos-arm64 .
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../site/downloads/build-agent-linux-x64 .
GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../site/downloads/build-agent-linux-arm64 .
(cd ../site/downloads && sha256sum build-agent-windows-*.exe build-agent-macos-arm64 build-agent-linux-x64 build-agent-linux-arm64 > build-agent.sha256)
ls -l ../site/downloads
