# Security Policy

updock talks to the Docker socket, which is root-equivalent on the host — we
take reports seriously.

## Reporting a vulnerability

Please use GitHub's **private vulnerability reporting**:
https://github.com/mdschoff/updock/security/advisories/new

Do not open public issues for security problems. You can expect an initial
response within a few days. Credit is given in the advisory unless you prefer
otherwise.

## Supported versions

Only the latest release receives security fixes.

## Scope notes

- updock never exposes a network listener; its only privileged interface is
  the Docker socket the operator mounts.
- Vulnerabilities in the Docker daemon itself or in container images updock
  manages are out of scope — report those upstream.
