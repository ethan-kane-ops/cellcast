# Releasing

A tag publishes five things, and every one of them is cut from the same commit.

| Artifact | Where it goes | Built by |
| --- | --- | --- |
| `cellcast-hub` image, linux/amd64 and linux/arm64 | `ghcr.io/ethan-kane-ops/cellcast-hub` | `Dockerfile` |
| `cellcast-agent` image, both architectures | `ghcr.io/ethan-kane-ops/cellcast-agent` | `Dockerfile` |
| `cellcast` client image, both architectures | `ghcr.io/ethan-kane-ops/cellcast` | `Dockerfile` |
| `cellcast` and `cellcast-agent` charts | `oci://ghcr.io/ethan-kane-ops/charts` | `helm package` |
| `cellcast` client archives and checksums | the GitHub release, and the Homebrew tap | goreleaser |

The client is on that list twice on purpose. A person installs it with `brew install`
or by unpacking an archive. A pipeline step that has no package manager, such as an
Argo CD PreSync hook, runs the image instead. The hub and the agent are the other way
round and ship only as images: they hold trust configuration and mint credentials, and
a tarball of the hub would look like a supported way to run the broker outside every
control the chart applies.

## One number

The git tag, both chart `appVersion` fields and every image tag are the same string.
The chart `version` fields are that string without the leading `v`, because Helm
requires bare semver there.

```
tag           v0.3.0
appVersion    v0.3.0     (also the default image tag)
version       0.3.0
image tag     v0.3.0
```

Helm lets a chart version and an appVersion move independently, and for a chart
packaged separately from the thing it installs that indirection earns its keep. It does
not here: one tag cuts the images and both charts from one commit, so a chart version
that did not match would name a release nothing else in the project has heard of.

`just release-version v0.3.0` writes all four numbers and regenerates the changelog.
A contract test holds the relationship, so a hand edit that breaks it fails `just
check` rather than reaching a registry.

## Cutting a release

```bash
just release-check              # the whole pipeline, publishing nothing
just release-version v0.3.0     # chart versions and CHANGELOG.md
git commit -am "chore(release): v0.3.0"
git tag -a v0.3.0 -m "v0.3.0"
just release v0.3.0
```

`just release` refuses to start unless the working tree is clean, the tag exists, the
tag points at `HEAD`, and both charts already declare it. Then it runs `just check-all`
and `just verify-e2e` before it publishes anything at all, because a credential broker
that shipped broken is not something to fix forward: by the time you notice, the bad
image has been pulled.

Publishing order is images, then charts, then the GitHub release. The release is the
artifact a person reads, and it should not appear before the things it describes exist.

### When a publish step fails halfway

Nothing about pushing three images, two charts and a GitHub release is atomic, so decide
in advance what a half-finished release means.

Before anyone has been told the release exists, re-run `just release v0.3.0`. Every step
is safe to repeat: the images and charts overwrite the same tag, and goreleaser is
configured with `mode: replace` so it rewrites the GitHub release rather than failing on
one that already exists.

Once the tag has been announced, or anything has pulled it, stop overwriting and cut a
patch tag instead. A tag whose contents changed after somebody pulled it is worse than a
version number nobody used, and this project's whole answer to "what am I running" is
that a digest names the same bytes tomorrow.

## Rehearsing it

`just release-check` runs every build the real thing runs and publishes none of it.
It is the local stand-in for a release workflow, and it is worth running before you
tag rather than after.

```bash
just release-check
```

Individual pieces, when something in there fails:

```bash
just image cellcast-hub          # one image, this machine's architecture, loaded locally
just images-check                # every image, every architecture, output discarded
just chart-package               # both charts into dist/charts
just release-notes v0.3.0        # what the GitHub release will say
just image-bases                 # current digests for the base images the Dockerfile pins
```

## Reproducible builds

Two builds of one commit produce identical bytes, and you can check that rather than
believing it:

```bash
just build && shasum -a 256 bin/cellcast
just build && shasum -a 256 bin/cellcast
```

Three things make that true. `-trimpath` keeps the absolute path of whoever built it
out of the binary. The base images are pinned by digest, not by tag, so rebuilding an
old tag uses the compiler that built it the first time. And the build stamps the
commit's own timestamp rather than the wall clock, which is the usual reason a
"reproducible" release turns out not to be.

The version a binary reports carries a `-dirty` suffix when the tree was not exactly a
commit. That suffix is the signal that its bytes are not reproducible by anybody else.

```
$ cellcast version
v0.3.0 (commit 4f2a1c9, built 2026-09-08T16:57:05+01:00, go1.26.8, darwin/arm64)
```

## Verifying what you install

Pin by digest in production. A tag can be moved; a digest names the same bytes
tomorrow. Both charts accept `image.digest`, which wins over `image.tag`:

```bash
helm install cellcast oci://ghcr.io/ethan-kane-ops/charts/cellcast \
  --set image.digest=sha256:...
```

Signatures and an SBOM are not part of this yet. They land with keyless cosign
signing, and until then the honest answer is that you are trusting the registry and the
digest.

## Why there is no workflow

CI is dormant until the repository goes public. Rather than a release that exists only
inside a workflow file, the pipeline is these recipes: `just release-check` rehearses
it and `just release` performs it, both from a laptop. When the workflow lands it calls
the same two recipes, which means the pipeline was rehearsable long before there was
anywhere to run it.
