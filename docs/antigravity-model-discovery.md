# Antigravity account model discovery

Enable account-scoped discovery to expose newly released upstream models without
editing the built-in catalog or rebuilding CPA. This affects Antigravity OAuth
credentials only. It does not bypass account selection, exclusions, cooldowns,
or registration, and does not configure downstream billing.

```yaml
antigravity-models:
  discovery-enabled: true
  refresh-interval: 3h
  additional-models:
    - id: actual-upstream-model-id
      display-name: Optional display name
      auth-ids: [stable-auth-id]
```

Use the credential's stable internal `id`, not `auth_index`, for `auth-ids`.
Omit `auth-ids` to add a model to all Antigravity accounts. IDs are exact upstream
identifiers, not display names. Aliases use `oauth-model-alias.antigravity`.
Manual entries survive discovery refreshes; deleting an entry removes it unless
that account also discovers it. Exclusions still win. Valid changes hot-reload;
invalid durations (less than one minute), empty IDs and whitespace-containing
IDs are rejected while the previous valid configuration stays active.

## Discovery and availability

Discovery runs asynchronously at startup and when accounts are added, then at
approximately the configured interval (default three hours). A one-minute scan
and deterministic early jitter spread refreshes; four concurrent metadata
requests are allowed. Failures retry with exponential backoff from one minute
to thirty minutes. Probes use each account's credentials, project and route/proxy
and have a 15-second metadata-work budget. Generation deadlines are unchanged.

The complete upstream `models` map supplies membership. The built-in catalog
supplies metadata for known IDs, not an allowlist. Unknown models retain their
exact ID and explicitly returned display name/input/output limits. Missing
limits remain unknown. Unknown thinking capabilities use the existing custom
model path: explicit parameters are handled by the adapter and validated
upstream. No thinking/search/tool mode is enabled by default. Search support
is advertised only when explicitly returned by the discovery endpoint.

HTTP errors, broken JSON and missing/null `models` retain the successful catalog.
A successful empty object removes automatic entries while keeping manual ones.
Before the first successful discovery, the static catalog is a temporary fallback.

Successful snapshots are saved atomically under `<auth-dir>/.antigravity-models/`.
They contain metadata and a hashed route identity, not tokens, and are isolated
by account, credential, project and route. Restart restores a matching snapshot
and refreshes asynchronously. Account deletion removes its snapshot. Refreshes
preserve cooldown/suspension state and reject stale account/config updates.
Discovery success does **not** prove inference success.

## Troubleshooting and rollback

- Success logs report model count, additions, removals and last success time.
- Failure logs report whether a snapshot is used and the next retry time.
  Check authentication, project and route connectivity; an unchanged list does
  not mean that fresh discovery succeeded.
- Check account scope, exclusions and aliases if a manual model is absent.
  Check the actual upstream ID if generation is rejected.
- Stale results for deleted/disabled accounts or changed routes/configs are discarded.
- `--local-model` controls the static updater, not this opt-in account discovery.
- Set `discovery-enabled: false` to hot-reload the original static strategy.
  Manual entries remain active. Remove `additional-models` as well for the original
  catalog alone. No database migration is required.

## Acceptance boundary

Tests use a local HTTP server and the real Antigravity executor, covering public
model listing, normal/streaming routing, isolation, manual entries, aliases,
exclusions, config validation, stale publication, failures and restart recovery.
They require no paid generation. Deployment and live inference acceptance are
separate steps.
