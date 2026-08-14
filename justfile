default:
    @just --list

build:
    CGO_ENABLED=0 go build -o bin/soft-kvm .

release:
    CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/soft-kvm .

test: test-unit

test-unit:
    go test -v ./...

lint:
    golangci-lint run ./... || true
    reuse lint --lines

# Local CI parity gate: lints, formatting, unit tests.
# `just ship` runs this before pushing.
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

# Push REV (default @-) as a PR on a <hostname>/<changeid> branch; auto-merges when CI passes.
# Runs check (lint + fmt-check + unit tests) before pushing.
ship rev="@-": check
    #!/usr/bin/env bash
    set -euo pipefail
    cid=$(jj log -r '{{rev}}' --no-graph -T 'change_id.short()')
    bookmark="$(hostname)/${cid}"
    jj bookmark set "$bookmark" -r '{{rev}}' --allow-backwards
    jj git push --bookmark "$bookmark"
    gh pr view "$bookmark" --json number >/dev/null 2>&1 \
        || gh pr create --head "$bookmark" --fill
    gh pr merge "$bookmark" --rebase --auto

# Fetch master and rebase the stack; merged changes and their bookmarks evaporate
sync:
    jj git fetch
    jj rebase -d 'trunk()' --skip-emptied

clean:
    rm -rf bin/
