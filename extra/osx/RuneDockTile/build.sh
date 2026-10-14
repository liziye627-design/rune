#!/bin/sh
# Copyright (C) 2017-2026 The Rune Authors
# SPDX-License-Identifier: GPL-3.0-or-later
#
# This program is free software: you can redistribute it and/or modify
# it under the terms of the GNU General Public License as published by
# the Free Software Foundation, either version 3 of the License, or (at
# your option) any later version.
#
# This program is distributed in the hope that it will be useful, but
# WITHOUT ANY WARRANTY; without even the implied warranty of
# MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU
# General Public License for more details.
#
# You should have received a copy of the GNU General Public License
# along with this program. If not, see <https://www.gnu.org/licenses/>.

# build.sh builds the Dock tile plug-in Rune.app ships, which the Dock
# loads while Rune is not running to show what the background workspace
# server keeps alive.
#
#   build.sh <arch> <min-macos> <out-dir>
#
# produces <out-dir>/RuneDockTile.docktileplugin for <arch> (arm64 or
# x86_64) with <min-macos> as its deployment target, which must be the
# app's own: the Dock refuses a plug-in newer than the app. VERSION and
# COMMIT in the environment are stamped into the bundle's Info.plist.
# The bundle is left unsigned; the app's codesign --deep signs it.
set -eu

if [ $# -ne 3 ]; then
  echo "usage: $0 <arch> <min-macos> <out-dir>" >&2
  exit 2
fi
arch=$1
min_macos=$2
out_dir=$3
src_dir=$(cd "$(dirname "$0")" && pwd)

bundle="$out_dir/RuneDockTile.docktileplugin"
rm -rf "$bundle"
mkdir -p "$bundle/Contents/MacOS"

sed -e "s/{{VERSION}}/${VERSION:-dev}/g" -e "s/{{COMMIT}}/${COMMIT:-dev}/g" \
  "$src_dir/Info.plist" > "$bundle/Contents/Info.plist"

xcrun clang \
  -fobjc-arc -bundle -Wall -Werror -O2 \
  -arch "$arch" -mmacosx-version-min="$min_macos" \
  -framework AppKit -framework Foundation \
  -o "$bundle/Contents/MacOS/RuneDockTile" \
  "$src_dir"/*.m

plutil -lint "$bundle/Contents/Info.plist" >/dev/null
echo "built $bundle"
