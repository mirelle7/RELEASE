# Build environment for the MinGW-w64 (i686) presets: a Linux container that cross-compiles the game for Windows.
# Everything here is open source, so unlike Visual C++ 6 and Visual Studio it can be rebuilt by anyone.
#
# NOTE: this recipe could not be test-built where it was written (package mirrors were not reachable).
# `build-agent --check-image` builds it on your machine and checks every tool the project needs.
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive HOME=/tmp

# cmake: Ubuntu 24.04 ships 3.28, the version the project asks for.
# wine64-tools + libwine-dev: widl (the IDL compiler EABrowserDispatch needs) and the Wine IDL headers.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates git cmake ninja-build make pkg-config python3 \
      gcc-mingw-w64-i686 g++-mingw-w64-i686 binutils-mingw-w64-i686 mingw-w64-i686-dev \
      wine64-tools libwine-dev \
 && rm -rf /var/lib/apt/lists/*

# POSIX threads variant of the cross compiler (std::thread and friends).
RUN update-alternatives --set i686-w64-mingw32-gcc /usr/bin/i686-w64-mingw32-gcc-posix \
 && update-alternatives --set i686-w64-mingw32-g++ /usr/bin/i686-w64-mingw32-g++-posix

# widl lives under /usr/lib/wine on some releases: make sure it is on PATH, and fail the image build if it is missing.
RUN (command -v widl >/dev/null || (for p in /usr/lib/wine/widl /usr/lib/wine-development/widl /usr/lib64/wine/widl; do [ -x "$p" ] && ln -s "$p" /usr/local/bin/widl && break; done)) \
 && command -v widl

# The source folder is a bind mount owned by the host user, which git refuses as "dubious ownership" (CMake could not
# check out its dependencies). Everything in this container is the player's own build, so trust any directory.
RUN git config --system --add safe.directory '*'

WORKDIR /src
