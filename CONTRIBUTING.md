# Contributing

Fellow nix users, a flake lives in `nix/dev`, use it with
`nix develop ./nix/dev` or `direnv allow`. On other systems:

```sh
# Go >= 1.24, just, dprint, reuse
```

Then `just` lists the recipes — `just test-unit`, `just fmt` and `just lint` are
the ones you'll want.

## Licensing

Dual-licensed **MIT OR Apache-2.0**. Commits must be signed off per the
[DCO](https://developercertificate.org/):

```sh
git commit -s
```

That's just a `Signed-off-by:` trailer — no GPG key needed.
