# ARC repository instructions

## Transport work

Before you add or change a transport, read
[the transport implementation contract](docs/delivery/TRANSPORTS.md).
Follow it for implementation, tests, documentation, and PR review.

- A transport moves signed ARC events through the existing delivery core.
- A codec, radio probe, echo service, or successful build is not a usable transport.
- Complete the requested ARC workflow through the actual transport and normal CLI.
- Check the callers as well as the adapter. Remove concrete relay assumptions
  only where the new transport needs integration.
- Reuse identity, verification, encryption, mail, and call behavior from core.
  Do not create a second implementation inside the adapter.
- Declare support for sync, live calls, and forwarding separately.
  Do not require a directory transport to support live calls.
- Keep a prerequisite PR explicitly partial. Link its integration work and name
  the remaining acceptance criteria. Do not present it as transport completion.
- Distinguish automated checks from tests on real devices.
  Report missing hardware proof before you claim the transport works.

## Technical language

Use short sentences and consistent terms in specifications and status reports.
Use ASD-STE100 writing rules where practical. Do not claim dictionary certification.
State what works, the evidence, and what remains unverified.
