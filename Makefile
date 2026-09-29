.PHONY: build \
	build-darwin-amd64 build-darwin-arm64 build-linux-amd64 build-linux-arm64 \
	amd arm cross build-cross cross-check \
	module-check shell-check maintenance-check test race vet staticcheck deadcode errcheck vuln check license-check \
	image-pipeline-test image-pipeline-native-test install-test \
	catalog-export catalog-keygen catalog-sign catalog-verify \
	release-check release-snapshot release-local gr-check gr-snapshot gr-local release-dev

SNAPSHOT_DIST ?= .goreleaser-snapshot

build:
	./packaging/build-dev.sh "$$(go env GOOS)" "$$(go env GOARCH)" bin

# Native macOS 27 companions and a verified local tarball, without publication.
.PHONY: mac-build mac-native-test
mac-build:
	bash ./packaging/build-mac.sh

mac-native-test:
	bash ./native/mac-runner/test.sh

build-darwin-amd64:
	./packaging/build-dev.sh darwin amd64 bin/darwin_amd64

build-darwin-arm64:
	./packaging/build-dev.sh darwin arm64 bin/darwin_arm64

build-linux-amd64:
	./packaging/build-dev.sh linux amd64 bin/linux_amd64

build-linux-arm64:
	./packaging/build-dev.sh linux arm64 bin/linux_arm64

amd: build-linux-amd64

arm: build-linux-arm64

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

staticcheck:
	staticcheck ./...

# The production-only diagnostic, `deadcode ./...`, is expected to report only
# YAML MarshalYAML/UnmarshalYAML hooks invoked through reflection. The enforced
# four-target test-linked intersection below must remain completely empty.
deadcode:
	@set -eu; \
	  temporary_parent=$$(cd "$${TMPDIR:-/tmp}" && pwd -P); \
	  temporary=$$(mktemp -d "$${temporary_parent}/barn-deadcode.XXXXXX"); \
	  cleanup() { \
	    case "$${temporary}" in \
	      "$${temporary_parent}"/barn-deadcode.*) rm -rf -- "$${temporary}" ;; \
	      *) printf 'refuse unsafe deadcode cleanup: %s\n' "$${temporary}" >&2 ;; \
	    esac; \
	  }; \
	  trap cleanup EXIT; \
	  for target in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64; do \
	    goos=$${target%-*}; goarch=$${target#*-}; \
	    GOOS=$${goos} GOARCH=$${goarch} deadcode -test ./... >"$${temporary}/$${target}.raw"; \
	    LC_ALL=C sort "$${temporary}/$${target}.raw" >"$${temporary}/$${target}"; \
	  done; \
	  comm -12 "$${temporary}/darwin-arm64" "$${temporary}/darwin-amd64" >"$${temporary}/intersection-2"; \
	  comm -12 "$${temporary}/intersection-2" "$${temporary}/linux-amd64" >"$${temporary}/intersection-3"; \
	  comm -12 "$${temporary}/intersection-3" "$${temporary}/linux-arm64" >"$${temporary}/intersection"; \
	  if test -s "$${temporary}/intersection"; then cat "$${temporary}/intersection"; exit 1; fi

errcheck:
	@output=$$(golangci-lint run --no-config --default=none -E errcheck \
	  --max-issues-per-linter=0 --max-same-issues=0 ./... 2>&1) || { printf '%s\n' "$${output}"; exit 1; }

vuln:
	govulncheck ./...

module-check:
	go mod verify
	go mod tidy -diff

shell-check:
	@for script in packaging/*.sh packaging/image-pipeline/*.sh tests/*.sh; do bash -n "$$script"; done

# A maintenance file must have an explicit owner in a Make target, workflow, or
# contributor documentation. Go tool tests use their package directory as the
# stable reference so adding another *_test.go does not require an inventory edit.
maintenance-check:
	@git ls-files 'tools/**' 'packaging/**' | while IFS= read -r path; do \
	  case "$$path" in tools/*/*.go) reference=$${path%/*} ;; *) reference=$$path ;; esac; \
	  grep -Fqs -- "$$reference" Makefile CONTRIBUTING.md .github/workflows/*.yml || { \
	    printf 'unowned maintenance file: %s\n' "$$path" >&2; exit 1; \
	  }; \
	done

cross: build-darwin-amd64 build-darwin-arm64 build-linux-amd64 build-linux-arm64

build-cross: cross

cross-check:
	GOOS=darwin GOARCH=arm64 go build ./...
	GOOS=darwin GOARCH=amd64 go build ./...
	GOOS=linux GOARCH=amd64 go build ./...
	GOOS=linux GOARCH=arm64 go build ./...

check: module-check shell-check maintenance-check test race vet staticcheck deadcode errcheck vuln cross-check image-pipeline-test install-test license-check

license-check:
	./packaging/verify-licenses.sh

image-pipeline-test:
	./tests/image-pipeline-test.sh

image-pipeline-native-test:
	BARN_IMAGE_PIPELINE_NATIVE_REQUIRED=1 ./tests/image-pipeline-native-test.sh

install-test:
	./tests/install-test.sh
	python3 ./tests/mac-release-test.py

catalog-export:
	@test -n "$(CATALOG_OUTPUT)" || { echo "CATALOG_OUTPUT is required" >&2; exit 2; }
	go run ./tools/catalogexport "$(CATALOG_OUTPUT)"

catalog-keygen:
	@test -n "$(CATALOG_KEY_DIR)" || { echo "CATALOG_KEY_DIR is required" >&2; exit 2; }
	@test -n "$(CATALOG_KEY_NAME)" || { echo "CATALOG_KEY_NAME is required" >&2; exit 2; }
	go run ./tools/catalogsign generate "$(CATALOG_KEY_DIR)" "$(CATALOG_KEY_NAME)"

catalog-sign:
	@test -n "$(CATALOG_KEY)" || { echo "CATALOG_KEY is required" >&2; exit 2; }
	@test -n "$(CATALOG_FILE)" || { echo "CATALOG_FILE is required" >&2; exit 2; }
	go run ./tools/catalogsign sign "$(CATALOG_KEY)" "$(CATALOG_FILE)"

catalog-verify:
	@test -n "$(CATALOG_PUBLIC_KEY)" || { echo "CATALOG_PUBLIC_KEY is required" >&2; exit 2; }
	@test -n "$(CATALOG_FILE)" || { echo "CATALOG_FILE is required" >&2; exit 2; }
	go run ./tools/catalogsign verify "$(CATALOG_PUBLIC_KEY)" "$(CATALOG_FILE)"

release-check:
	./packaging/check-toolchain.sh goreleaser
	goreleaser check

release-snapshot: release-check
	@normalized=$$(printf '%s' "$(SNAPSHOT_DIST)" | tr '[:upper:]' '[:lower:]'); \
	  case "$$normalized" in ""|*/*|.goreleaser-dist|.goreleaser-companion|.goreleaser-licenses) \
	  echo "SNAPSHOT_DIST must be one new directory directly under the repository root" >&2; exit 2 ;; \
	  esac
	@test ! -e "$(SNAPSHOT_DIST)" || { echo "refuse existing snapshot output: $(SNAPSHOT_DIST)" >&2; exit 1; }
	@test ! -e .goreleaser-dist || { echo "refuse existing GoReleaser work output: .goreleaser-dist" >&2; exit 1; }
	@test ! -e .goreleaser-companion -a ! -L .goreleaser-companion || { echo "refuse existing GoReleaser companion stage: .goreleaser-companion" >&2; exit 1; }
	@test ! -e .goreleaser-licenses -a ! -L .goreleaser-licenses || { echo "refuse existing generated license stage: .goreleaser-licenses" >&2; exit 1; }
	@set -e; \
	  source_epoch=$${SOURCE_DATE_EPOCH:-$$(git show -s --format=%ct HEAD)}; \
	  test -n "$$source_epoch" || { echo "cannot resolve SOURCE_DATE_EPOCH" >&2; exit 2; }; \
	  commit=$$(git rev-parse --verify HEAD); \
	  if ! git diff --quiet || ! git diff --cached --quiet || test -n "$$(git status --short --untracked-files=normal)"; then commit=uncommitted; fi; \
	  if build_date=$$(date -u -r "$$source_epoch" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null); then :; \
	  else build_date=$$(date -u -d "@$$source_epoch" +%Y-%m-%dT%H:%M:%SZ); fi; \
	  cleanup_companions() { \
	    ./packaging/goreleaser-companion.sh cleanup darwin amd64 .goreleaser-companion; \
	    ./packaging/goreleaser-companion.sh cleanup darwin arm64 .goreleaser-companion; \
	    ./packaging/goreleaser-companion.sh cleanup linux amd64 .goreleaser-companion; \
	    ./packaging/goreleaser-companion.sh cleanup linux arm64 .goreleaser-companion; \
	    ./packaging/dependency-licenses.sh clean .goreleaser-licenses; \
	    if test -e .goreleaser-dist; then rm -rf -- .goreleaser-dist; fi; \
	  }; \
	  trap cleanup_companions EXIT; \
	  SOURCE_DATE_EPOCH="$$source_epoch" BARN_COMMIT="$$commit" BARN_BUILD_DATE="$$build_date" \
	    goreleaser release --snapshot --parallelism 1; \
	  snapshot_version=$$(jq -er '.version | strings | select(length > 0)' .goreleaser-dist/metadata.json); \
	  if test -n "$${BARN_MAC_PAYLOAD:-}"; then \
	    python3 packaging/mac-release.py attach "$$snapshot_version" "$$commit" "$$source_epoch" .goreleaser-dist "$$BARN_MAC_PAYLOAD"; \
	  fi; \
	  SOURCE_DATE_EPOCH="$$source_epoch" ./packaging/verify-goreleaser.sh "$$snapshot_version" "$$PWD/.goreleaser-dist"; \
	  ./packaging/verify-linux-packages.sh "$$snapshot_version" "$$commit" "$$source_epoch" "$$PWD/.goreleaser-dist"; \
	  mv .goreleaser-dist "$(SNAPSHOT_DIST)"

release-local:
	@test -n "$(VERSION)" || { echo "VERSION is required" >&2; exit 2; }
	@if test -n "$(OUTPUT)"; then \
	  ./packaging/release-local.sh "$(VERSION)" "$(OUTPUT)"; \
	else \
	  ./packaging/release-local.sh "$(VERSION)"; \
	fi

gr-check: release-check

gr-snapshot: release-snapshot

gr-local: release-local

release-dev:
	@test -n "$(VERSION)" || { echo "VERSION is required" >&2; exit 2; }
	@test -n "$(SOURCE_DATE_EPOCH)" || { echo "SOURCE_DATE_EPOCH is required" >&2; exit 2; }
	SOURCE_DATE_EPOCH="$(SOURCE_DATE_EPOCH)" ./packaging/build-release.sh "$(VERSION)" "dist/$(VERSION)"
	./packaging/verify-release.sh "$(VERSION)" "$(CURDIR)/dist/$(VERSION)"
