# Branch and release protection

Protect the default `main` branch. Require these stable checks before merge:

- `ci`
- `container`
- `codeql`
- `kind`

Require pull requests, current branch heads, and one review. Restrict bypasses to repository administrators and release maintainers.

Protect the `v*` tag namespace with a ruleset. Only the SSH signer in `.github/release-signers` may create tags. Deny tag updates and deletions.

The release workflow is the only actor allowed to write GHCR version tags and GitHub release assets. Its `publish` job is the only write-permission job; all other jobs remain read-only.

External acceptance remains pending until a protected, SSH-signed `v*` tag completes. Verify the live branch/tag rulesets, published archives and checksums/SBOMs, both GHCR platforms, and installation from the unmodified attached cask.
