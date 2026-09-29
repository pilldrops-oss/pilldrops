# Security Policy

PillDrops runs untrusted code inside isolated environments, so we take security
reports seriously and appreciate responsible disclosure.

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues,
discussions, or pull requests.**

Instead, open a private report through GitHub's
[private vulnerability reporting](https://github.com/pilldrops-oss/pilldrops/security/advisories/new)
("Report a vulnerability" under the repository's **Security** tab). Only the
maintainers can see it.

Please include:

- a description of the issue and its impact,
- the affected component (control plane, node agent, a specific runtime, envd,
  etc.) and version/commit,
- steps to reproduce or a proof of concept, and
- any suggested remediation.

We will acknowledge your report, work with you to understand and validate it,
and keep you informed of remediation progress. Please give us a reasonable
window to release a fix before any public disclosure.

## Scope

Because this platform executes untrusted workloads, we are especially
interested in reports involving:

- cross-tenant escape or reachability (VM↔VM, container↔container, guest↔host),
- privilege escalation from a sandbox to the node,
- authentication/authorization bypass in the control plane or node API,
- egress controls (e.g. reaching the node agent, internal ranges, or cloud
  metadata endpoints from a sandbox), and
- supply-chain integrity of the boot artifacts (kernel, VMM, jailer).

## Supported versions

Security fixes are applied to the latest release / `main`. Older versions are
not maintained.

## Threat model

The threat model — what each tier can do to the ones below it, every credential
and what it binds to, which properties are verified rather than assumed, and the
open findings with their severity — is **not published in this repository**. It
is kept with the operational notes because it names live hosts. Ask on a
security advisory if you need it for a review.

What does ship is the part you can run:

`scripts/security-suite.sh --runtime <name> --strict` re-runs the reachability
and containment checks against a live node; a probe that could not run reports
as skipped with a reason, never as a pass.

## Upgrading an existing fleet

Before rolling a node onto a build newer than 2026-08-31: several hardening
changes are **breaking for a fleet that already exists** and silent for a fresh
install — a Postgres grant script that used to abort halfway, an update path that
now refuses to run without cosign, per-node credentials that no longer fall back,
and tenant code that no longer runs as root.

`sudo scripts/pre-upgrade-check.sh` answers the machine-checkable half on a
node. It exits 2 for a check it could not run, which is not a pass.
