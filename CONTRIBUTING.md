# Contributing to wxingress

Thank you for considering a contribution. wxingress is a drop-in successor of ingress-nginx built on NGINX
Ingress Controller, and contributions that keep it compatible, correct and fast are very welcome.

## Questions and ideas

- Questions and ideas: [GitHub Discussions](https://github.com/wx-one/wxingress/discussions)
- Bugs and concrete feature requests: [issues](https://github.com/wx-one/wxingress/issues)
- Security issues: see [SECURITY.md](SECURITY.md), never a public issue

An ingress-nginx behaviour that wxingress does not reproduce is a bug. Please include the Ingress (or a
reduced version of it), the request, and what ingress-nginx answered compared to wxingress.

## Development

How to build and test the controller, the data plane module and the image is described in
[Building](https://wx-one.github.io/wxingress/developer/building/). In short:

```sh
make secrets
make test
```

Before you open a pull request:

- `gofmt` and `make test` pass. After template changes, the snapshots are updated (`make test-update-snaps`)
  and a fixture actually renders the new directive.
- After a change to `pkg/apis/configuration/v1/types.go`, `make update-codegen` and `make update-crds` were
  run and their output is committed. Generated code is never edited by hand.
- Nothing below `internal/third_party/ngf/` is edited by hand; see
  [Upstream](https://wx-one.github.io/wxingress/developer/upstream/).
- A change of ingress-nginx compatibility comes with a test, and the documentation in `docs/` says what
  changed.

## Commits and pull requests

- One topic per pull request. Small pull requests are reviewed faster.
- Commit messages say what changed and why, in plain English. Prefix them with `fix:`, `feat:`, `docs:`,
  `test:`, `chore:` or `deploy:`.
- Contributions are licensed under the [EUPL-1.2](LICENSE), the project's licence. By opening a pull request
  you confirm that you have the right to contribute the code under that licence.

## Code of conduct

Everyone taking part in the project is expected to follow the [code of conduct](CODE_OF_CONDUCT.md).
