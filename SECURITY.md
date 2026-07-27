# Security policy

## Supported versions

Security fixes are provided for the latest released version of Baseten Switch.
Pre-release builds and older versions may be used to reproduce a report, but
are not supported release lines.

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability.

Use GitHub's
[private vulnerability reporting form](https://github.com/basetenlabs/baseten-switch/security/advisories/new)
to send the report directly to the repository's security maintainers. Include:

- the affected version and operating system;
- the expected and observed behavior;
- the security impact;
- the smallest safe reproduction;
- any suggested mitigation.

Do not include live API keys, access tokens, prompts, customer data, or other
secrets. Replace them with synthetic values.

The maintainers will coordinate validation, remediation, disclosure, and
credit through the private advisory. Please allow time for a fix before
publishing report details.

## Security boundary

Baseten Switch handles provider credentials and coding-harness traffic. Reports
about credential exposure, unintended network destinations, prompt or response
disclosure, unsafe local file mutation, localhost access control, release
signing, or update integrity are in scope.

General support questions, feature requests, and non-security defects belong
in the public issue tracker.
