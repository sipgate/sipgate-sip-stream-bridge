# Security Policy

## Reporting a Vulnerability

Please do **not** open a public GitHub issue for security vulnerabilities.

Instead, send a report to **security@sipgate.de** with:
- A description of the vulnerability
- Steps to reproduce
- Potential impact assessment
- Any suggested mitigations (optional)

We will acknowledge your report within **5 business days** and aim to release a fix within **30 days** for confirmed vulnerabilities.

There is currently no bug bounty programme.

## Scope

This project is a SIP-to-WebSocket media bridge. Areas of particular interest:
- SIP message handling and parsing
- WebSocket stream authentication and isolation
- RTP media routing between calls
- Credential handling via environment variables
