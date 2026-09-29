#!/bin/bash
# Publish a verified local app by renaming complete directories. This never
# installs a service or launches the app; the previous app is removed only
# after its replacement is in place.
set -euo pipefail
if [[ $# != 2 ]]; then echo "usage: $0 SOURCE_APP DESTINATION_APP" >&2; exit 2; fi
repo="$(cd "$(dirname "$0")/.." && pwd -P)"
source_app=$1
destination=$2
[[ $destination == /* ]] || destination="$repo/$destination"
case /$destination/ in */../*|*/./*) echo "Refusing dot path in app destination" >&2; exit 2 ;; esac
case $destination in "$repo"/bin/*.app) ;; *) echo "Local app destination must be inside repository bin" >&2; exit 2 ;; esac
parent=${destination%/*}
[[ -d $parent && ! -L $parent && $(cd "$parent" && pwd -P) == "$parent" ]] || { echo "Unsafe app destination parent: $parent" >&2; exit 2; }
[[ ! -L $destination && ( ! -e $destination || -d $destination ) ]] || { echo "Unsafe app destination: $destination" >&2; exit 2; }
[[ -d $source_app && ! -L $source_app ]] || { echo "App source must be a real directory" >&2; exit 2; }
codesign --verify --deep --strict "$source_app"
stage="$(mktemp -d "$parent/.mac-app-stage.XXXXXXXX")"
cleanup() { case $stage in "$parent"/.mac-app-stage.*) rm -rf -- "$stage" ;; esac; }
trap cleanup EXIT
ditto "$source_app" "$stage/Barn Mac.app"
codesign --verify --deep --strict "$stage/Barn Mac.app"
previous=
if [[ -e $destination ]]; then
  previous="$(mktemp -d "$parent/.mac-app-previous.XXXXXXXX")"
  mv "$destination" "$previous/Barn Mac.app"
fi
if ! mv "$stage/Barn Mac.app" "$destination"; then
  if [[ -n $previous && ! -e $destination ]]; then mv "$previous/Barn Mac.app" "$destination"; fi
  exit 1
fi
case $previous in "$parent"/.mac-app-previous.*) rm -rf -- "$previous" ;; esac
printf 'Native app: %s\n' "$destination"
