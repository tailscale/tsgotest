# tsgotest

This is a test suite for Tailscale's Go fork (https://github.com/tailscale/go/)

When we rebase [our Go patches](https://github.com/tailscale/go/issues?q=is%3Aissue%20state%3Aopen%20label%3Atsgo) every six months,
this test suite makes sure we didn't drop anything we depend on elsewhere.

(Tailscale's policy is that our open source repo must support the
latest stock Go toolchain, but our internal private repos can require
Tailscale's Go)
