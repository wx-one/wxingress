# Security policy

## Supported versions

Security fixes are released for the latest wxingress release. wxingress tracks NGINX's security releases:
when nginx.org publishes a fix for NGINX, a wxingress release with the fixed NGINX follows, usually within
about 24 to 48 hours on working days.

Customers with a [support package](https://wx-one.github.io/wxingress/about/support/) also get targeted
fixes for critical vulnerabilities through the fast release channel, run by the WX-ONE emergency response
team, before the official release.

## Reporting a vulnerability

We follow **responsible disclosure**. Please do not report security issues in public issues, discussions
or pull requests, and do not publish details before a fix is available.

Report vulnerabilities by email to **[security-ingress@wx1.de](mailto:security-ingress@wx1.de)**, or
through GitHub's [private vulnerability reporting](https://github.com/wx-one/wxingress/security/advisories/new).
Please include:

- what you found and its impact,
- how to reproduce it (a minimal Ingress, ConfigMap or request, if possible),
- the wxingress version or image tag you tested,
- whether and when you intend to publish, and how you would like to be credited.

What you can expect from us:

1. We acknowledge your report within **three working days**.
2. We confirm or rule out the issue, assess its severity and keep you informed while we work on a fix.
3. We release the fix and publish a security advisory, crediting you unless you prefer otherwise.
4. We ask you to keep the details confidential until the advisory is published. We aim to fix critical
   issues within days and agree on a disclosure date with you; 90 days after the report is the latest
   unless we agree otherwise.

Please test only against systems you own or are allowed to test, do not access or modify data of others,
and do not degrade services for other users.

Vulnerabilities in code that wxingress inherits from NGINX Ingress Controller, NGINX Gateway Fabric or
NGINX itself may also concern F5. If you report one to us, we coordinate the disclosure with them.
