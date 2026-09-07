# CPA deployment to 216

The production source branch is `main` in the `zyaireleo/CLIProxyAPI` fork. A push to that branch runs `.github/workflows/deploy-216.yml` after tests and a Linux dynamic CGO build pass. The workflow also supports a manual deploy from `main`.

Configure these repository secrets before enabling the workflow:

- `CPA_216_HOST`
- `CPA_216_PORT`
- `CPA_216_USER`
- `CPA_216_SSH_KEY`
- `CPA_216_KNOWN_HOSTS`

The target account must be able to run the deployment script with `sudo`. The script uploads an immutable archive under `/opt/cliproxyapi/incoming`, verifies its SHA-256, checks that the binary is dynamically linked and resolvable by `ldd`, then atomically switches `/opt/cliproxyapi/current` and restarts `cliproxyapi.service`. The previous symlink target is retained and restored automatically if restart or health checks fail.

The workflow does not modify `/etc/cliproxyapi/config.yaml`, account data, or database state.
