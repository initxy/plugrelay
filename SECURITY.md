# Security policy

## Reporting a vulnerability

Report privately through GitHub's "Report a vulnerability" (Security
Advisories) on this repository. Include a description, affected versions and,
if possible, a minimal reproduction. We aim to acknowledge within 3 working
days and to ship a fix or mitigation within 30 days for high-severity issues.

## Supported versions

plugrelay is pre-1.0. Only the latest release gets security fixes; upgrade
to it before reporting, if you can.

## Scope

In scope: authentication and key handling, credential encryption, the egress
path (SSRF, header leakage, proxy fallback), the external plugin boundary
(host APIs, supervision), request/response tampering, billing integrity.

Out of scope: vulnerabilities in upstream providers, and deployments that
expose the admin console publicly against the documented defaults.

## Hardening defaults

- Listens on 127.0.0.1 unless `listen` in relay.yaml says otherwise.
- No account has a default password. `plugrelay init --admin-email` creates
  an admin whose password comes from `--admin-password`,
  `PLUGRELAY_ADMIN_PASSWORD` or stdin. Without `--admin-email`, `init`
  creates a local owner account (`owner@localhost`, random password never
  shown) that the console signs in as without a login, only for loopback
  requests while the relay listens on loopback and the site setting
  `security.local_owner_login` is on ([API auth](docs/api.md#authentication)).
  A non-loopback `--listen` needs `--admin-email` (asked for when `init`
  runs interactively) or `--no-admin`; with `--no-admin` no account exists and the console's
  first-run setup creates the admin
  ([deployment](docs/deployment.md#local-single-user)).
- Platform keys are stored as hashes; upstream credentials are encrypted with
  the master key.
- Releases ship with checksums, cosign signatures and an SBOM.
