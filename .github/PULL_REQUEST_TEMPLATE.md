### What this changes

Describe the change and why it is needed. If it addresses an issue, link it
here (for example `Fixes #123`), not in the title.

### How it was tested

Describe how you verified the change: unit or snapshot tests, a run on a
cluster, manifests used.

### Checklist

- [ ] I have read [CONTRIBUTING.md](https://github.com/wx-one/wxingress/blob/main/CONTRIBUTING.md)
- [ ] `make test` passes; snapshots are regenerated with `make test-update-snaps` where templates changed
- [ ] Generated code and CRDs are up to date (`make update-codegen`, `make update-crds`) where types changed
- [ ] Tests cover the change
- [ ] Documentation is updated where behaviour or configuration changed
- [ ] The branch is rebased onto `main`
