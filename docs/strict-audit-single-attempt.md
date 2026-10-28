# Strict audit generation attempts

Authenticated requests marked `X-Sub2API-Audit-Hop: strict_v1` share a single Gemini generation attempt budget for the entire HTTP request. Credential rotation, request retry rounds, credits fallback and stream bootstrap cannot dispatch a second generation call. The existing generation budget enforces this at the outgoing request boundary.

Ordinary generation requests retain their existing retry and attempt configuration. The audit header only reduces attempts; it does not bypass authentication, scheduling or safety policy. Each independently audited input chunk is a separate HTTP request. Sub2API stops auditing on its first failed chunk and degrades business forwarding according to its existing policy.
