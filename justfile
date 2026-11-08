default:
    @just --list

build:
    CGO_ENABLED=0 go build -o soft-kvm .

release:
    CGO_ENABLED=0 go build -ldflags="-s -w" -o soft-kvm .

test: test-unit

test-unit:
    go test -v ./...

lint:
    golangci-lint run ./... || true
    reuse lint --lines

# Lint, check formatting, run unit tests. `just ship` and unstable.yml run it.
check: lint fmt-check test-unit

fmt:
    go fmt ./... 2>/dev/null || true
    dprint fmt

fmt-check:
    test -z "$(gofmt -l . 2>/dev/null)"
    dprint check

# Configure git to use hooks from the .githooks directory
setup-hooks:
    git config core.hooksPath .githooks

# Check, rebase onto master, and push REV (default: @ if non-empty, else @-) to master.
ship rev="heads(::@ & ~empty())": check
    #!/usr/bin/env bash
    set -euo pipefail
    target=$(jj log -r '{{rev}}' --no-graph -T 'change_id.short()')
    jj git fetch
    jj rebase -b "$target" -d 'trunk()' --skip-emptied
    if [ -z "$(jj log -r "trunk()..$target" --no-graph -T 'change_id')" ]; then
        echo "Nothing between trunk() and $target to ship." >&2
        exit 1
    fi
    if [ -n "$(jj log -r "trunk()..$target & description(exact:'')" --no-graph -T 'change_id')" ]; then
        echo "Refusing to ship commit with empty description in trunk()..$target." >&2
        exit 1
    fi
    jj bookmark set master -r "$target"
    jj git push --bookmark master

# Fetch master and rebase the stack; merged changes and their bookmarks evaporate
sync:
    jj git fetch
    jj rebase -d 'trunk()' --skip-emptied

clean:
    rm -f soft-kvm
