# Security Policy

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| 0.1.x   | :white_check_mark: |

Security fixes are applied to the latest release on the `main` branch.

## Reporting a Vulnerability

Please **do not** open a public issue for security vulnerabilities.

Report privately via GitHub Security Advisories:
**https://github.com/technothatch/beast-relay/security/advisories/new**

Or email **security@technothatch.dev** with:

- A description of the issue and its impact
- Steps to reproduce, or a proof of concept
- Affected versions / commit SHA

You should receive an acknowledgement within 72 hours. We will work with you on
a coordinated disclosure and credit you in the advisory unless you prefer
anonymity.

## Threat model notes

These tools consume and forward ADS-B data, which is untrusted, broadcast
radio input. Specifically:

- All decoder output (`aircraft.json`, Beast, SBS, AVR) is treated as
  **untrusted input**: parsers must never crash, allocate unboundedly, or
  execute anything derived from the data.
- URLs, hostnames and command templates come from local configuration files,
  never from decoder output.
- The `shell`/`restart` style integrations must be explicitly enabled in
  configuration; they are off by default.
- Feeds are forwarded as opaque bytes; no TLS termination or authentication is
  implied unless explicitly documented.
