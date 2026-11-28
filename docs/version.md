# tvt version

`tvt version` prints the release tag, the commit and the build time of the binary. It takes no flags and talks to nothing.

```sh
$ tvt version
tvt 0.1.0-rc1 (commit befb021, built 2026-11-21T14:31:18Z)
```

The commit and time come from the release build, which is reproducible: a clean checkout of the tag builds the same binary (see [install.md](install.md)).

A binary built with `go install` or `go build` prints `tvt dev (commit none, built unknown)`. To run a version you can verify, use a release archive and check it as [install.md](install.md) describes.
