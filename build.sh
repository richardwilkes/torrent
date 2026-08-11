#! /usr/bin/env bash

set -eo pipefail

trap 'echo -e "\033[33;5mBuild failed on build.sh:$LINENO\033[0m"' ERR

LINT=0
TEST=0
RACE=0

for arg in "$@"
do
  case "$arg" in
    --all|-a) LINT=1; TEST=1; RACE=1 ;;
    --lint|-l) LINT=1 ;;
    --race|-r) TEST=1; RACE=1 ;;
    --test|-t) TEST=1 ;;
    --help|-h)
      echo "$0 [options]"
      echo "  -a, --all  Equivalent to --lint --test --race"
      echo "  -l, --lint Run the linters"
      echo "  -r, --race Run the tests with race-checking enabled"
      echo "  -t, --test Run the tests"
      echo "  -h, --help This help text"
      exit 0
      ;;
    *)
      echo "Invalid argument: $arg"
      exit 1
      ;;
  esac
done

# Build the code
echo -e "\033[33mBuilding Go code...\033[0m"
go build -v ./...

# Run the linters
if [ "$LINT" == "1" ]; then
  TOOLS_DIR=$(go env GOPATH)/bin
  INSTALLED_VERSION=
  if [ -x "$TOOLS_DIR/golangci-lint" ]; then
    INSTALLED_VERSION=$("$TOOLS_DIR/golangci-lint" version 2>&1 | awk '{ print $4 }' || true)
  fi
  # Determining the latest version requires network access, so tolerate the lookup failing and fall back to whatever is
  # already installed rather than aborting the whole script when offline.
  LATEST_VERSION=$(curl --head -s https://github.com/golangci/golangci-lint/releases/latest | grep -i location: | sed 's/^.*v//' | tr -d '\r\n' || true)
  if [ -z "$LATEST_VERSION" ]; then
    if [ -z "$INSTALLED_VERSION" ]; then
      echo "Unable to determine the latest golangci-lint version and no usable copy is installed in $TOOLS_DIR"
      exit 1
    fi
    echo -e "\033[33mUnable to determine the latest golangci-lint version; using the installed $INSTALLED_VERSION\033[0m"
  elif [ "$INSTALLED_VERSION" != "$LATEST_VERSION" ]; then
    echo -e "\033[33mInstalling version $LATEST_VERSION of golangci-lint into $TOOLS_DIR...\033[0m"
    mkdir -p "$TOOLS_DIR"
    curl -sfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b "$TOOLS_DIR" "v$LATEST_VERSION"
  fi
  echo -e "\033[33mLinting...\033[0m"
  "$TOOLS_DIR/golangci-lint" run
fi

# Run the tests
if [ "$TEST" == "1" ]; then
  TEST_ARGS=()
  if [ "$RACE" == "1" ]; then
    TEST_ARGS+=(-race)
    echo -e "\033[33mTesting with -race enabled...\033[0m"
  else
    echo -e "\033[33mTesting...\033[0m"
  fi
  go test "${TEST_ARGS[@]}" ./...
fi
