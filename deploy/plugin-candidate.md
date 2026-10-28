# Plugin-capable candidate verification

This workflow only builds and verifies an artifact. It does not transfer files to
production, change a release pointer, reload Nginx, or restart a service.

The candidate preserves the safety branch source and embedded model catalog.
`deploy/build-plugin-candidate.sh` pins Go 1.27.0 and Linux amd64 CGO builds. It
refuses changed tracked source, existing output, or an explicit CGO=0 request.
`deploy/verify-plugin-artifact` reads Go build information and ELF metadata without
executing the artifact. Its default glibc ceiling is 2.35, matching the diagnosed
Ubuntu 22.04 production host. This check does not prove a plugin ABI is compatible.

## Local Linux validation

From a committed safety-based branch, with Go 1.27.0 and GCC on PATH:

```sh
CGO_ENABLED=1 go test ./...
bash deploy/build-plugin-candidate.sh /absolute/path/to/new-candidate
```

Use `go test ./internal/pluginhost -run '^TestDynamicLibraryLoaderUnix' -count=1 -v`
for the native C ABI fixture tests. They compile real shared libraries and exercise
the Unix loader. `CI=true` makes a missing compiler an error rather than a skip.

For compatibility with the four installed plugin versions, place their local
`.so` files in an isolated directory. `deploy/plugin-smoke-manifest.json` pins
both the public release archive hash and the installed shared-library hash. The
CI workflow downloads literal, versioned public URLs; extraction verifies both
hashes and does not use paths from the archive as output filenames.

```sh
sudo unshare --net --pid --fork --mount-proc bash -c '
  set -euo pipefail
  ip link set lo up
  python3 deploy/plugin-smoke.py \
    --binary /absolute/path/to/new-candidate/cli-proxy-api \
    --plugins /absolute/path/to/pinned-plugins \
    --workdir /absolute/path/to/new-offline-state \
    --report /absolute/path/to/new-candidate/plugin-smoke.json
'
```

The namespace must contain only loopback. The smoke test uses a new empty auth
directory, random test-only keys, private caches and plugin state, and a sanitized
child environment. It never copies production config or OAuth material. It checks
the capability header, actual registrations and effective enablement, mapped
shared libraries, the priority plugin routes, and the retained safety mode. The
four request-construction safety tests separately verify the wire semantics.

The disposable candidate is killed as a whole at the end; this does not test
in-process plugin unload or its separate late-callback risks. The temporary
configuration containing the random keys is removed. The plugin snapshots and
SQLite data, if created, exist only in the isolated empty-account state directory.

## Production boundary

A successful report is not authorization to deploy. The legacy `deploy/216`
helper still assumes an obsolete local Sub2API/database topology and must not be
invoked on the current host as part of validation. Production transition requires
its own reviewed CI path, a single OAuth/refresh owner, approved drain/maintenance
budgets, and a rollback to the immediately preceding safety release. Do not run a
second candidate against the same production auth files. Do not use old native
scheduler releases, overwrite current auth snapshots, or force a plugin support
header to hide a loader failure.
