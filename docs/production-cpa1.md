# CPA1 production source baseline

Verified on 2026-11-12. Select a release from the live service version before
building an artifact; the fork default branch does not establish the production
version.

## Access route

Use Tailscale SSH to the HK38 jump host, then the established server access
configuration to reach CPA1 over the tailnet. Resolve `<HK38_TAILNET_HOST>` and
`<CPA1_TAILNET_HOST>` from private operations notes. Do not store their actual
addresses, hostnames or authentication material in this public repository.

For example: `tailscale ssh root@<HK38_TAILNET_HOST>`.
A local private key is not required for the Tailscale SSH jump-host login.

## Verified CPA1 layout and source

| Property | Verified value or template |
| --- | --- |
| Target host | `<CPA1_TAILNET_HOST>` |
| Systemd unit | `cliproxyapi1.service` |
| Listener port | `8316` |
| Executable pointer | `/opt/cliproxyapi/current-cpa1/server` |
| Existing release executable | `/opt/cliproxyapi/releases/cpa-v8.0.66-official/server` |
| Official source tag | `router-for-me/CLIProxyAPI` `v8.0.66` |
| Official source commit | `fa11a573b252b30256bef4de952a95feee3c8c9d` |
| Target GLIBC | `2.36` |

The fork branch `production/cpa1-v8.0.66` starts at that exact official commit.
The Antigravity isolation change is based on this version and defaults off.

Older fork `main` deployment notes describing CPA1 as a v7 service on port 8317
are stale for this verified layout. Do not deploy their v7 artifacts to CPA1.
Keep the existing service and executable-pointer layout when preparing the
version-matched deployment.
