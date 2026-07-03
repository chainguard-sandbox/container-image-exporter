# Commit conventions

Use [Conventional Commits](https://www.conventionalcommits.org/) for all commit messages and PR titles. This is enforced by the release workflow, which derives semver bumps from commit history.

Format: `<type>(<optional scope>): <description>`

Common types:
- `feat`: new feature (minor bump)
- `fix`: bug fix (patch bump)
- `docs`: documentation only
- `chore`: tooling, deps, or other non-code changes
- `refactor`: code change that neither fixes a bug nor adds a feature
- `test`: adding or fixing tests
- `ci`: CI configuration changes
- `perf`: performance improvement

Breaking changes: append `!` after the type/scope (e.g. `feat!:`) or include a `BREAKING CHANGE:` footer to trigger a major bump.

Keep the subject line under ~72 characters and in the imperative mood.
