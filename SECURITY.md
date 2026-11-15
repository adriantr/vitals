# Security policy

## Reporting a vulnerability

Report vulnerabilities privately through GitHub:
[Report a vulnerability](https://github.com/adriantr/vitals/security/advisories/new). Please do not
open a public issue for them.

Include what you found, how to reproduce it, and the version (`vitals version`). You should get a
first answer within a week. Once a fix is released, the advisory is published with credit, unless
you prefer otherwise.

## Supported versions

Fixes go into the latest release only.

## Scope

vitals reads cloud APIs with the caller's credentials and prints what it reads. Relevant issues
include leaking a token (into output, logs or errors), reading outside the configured projects,
and code execution through a crafted config or repository. vitals never writes to the APIs it reads.
