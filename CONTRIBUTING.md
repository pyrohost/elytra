# Contributing to Elytra

Thank you for your interest in contributing to Elytra! This document provides guidelines for contributing to the project.

## Development Setup

1. Fork the repository
2. Clone your fork: `git clone https://github.com/yourusername/elytra.git`
3. Create a feature branch: `git checkout -b feature/your-feature-name`
4. Make your changes
5. Test your changes: `go test ./...`
6. Commit your changes using conventional commits (see below)
7. Push to your fork and create a pull request

## Conventional Commits

This project uses [Conventional Commits](https://www.conventionalcommits.org/) for automatic versioning and changelog generation. All commit messages must follow this format:

```none
<type>[optional scope]: <description>

[optional body]

[optional footer(s)]
```

### Commit Types

- **feat**: A new feature (triggers MINOR version bump)
- **fix**: A bug fix (triggers PATCH version bump)
- **docs**: Documentation only changes (no version bump)
- **style**: Changes that do not affect the meaning of the code (no version bump)
- **refactor**: A code change that neither fixes a bug nor adds a feature (no version bump)
- **perf**: A performance improvement (triggers PATCH version bump)
- **test**: Adding missing tests or correcting existing tests (no version bump)
- **chore**: Changes to the build process or auxiliary tools (no version bump)

### Breaking Changes

To indicate breaking changes, add an exclamation mark after the type:

- **feat!**: A new feature with breaking changes (triggers MAJOR version bump)
- **fix!**: A bug fix with breaking changes (triggers MAJOR version bump)

You can also include `BREAKING CHANGE:` in the commit body or footer.

### Examples

```bash
# Feature addition (minor bump)
feat: add support for rustic backup encryption

# Bug fix (patch bump)
fix: resolve server startup crash on invalid config

# Breaking change (major bump)
feat!: change default configuration format from TOML to YAML

# Breaking change with body
feat: add new authentication system

BREAKING CHANGE: the old authentication tokens are no longer supported
```

### Scopes

Optionally, you can add a scope to provide additional context:

- `feat(backup): add compression options`
- `fix(sftp): resolve permission issues`
- `docs(readme): update installation instructions`

## Git Message Template

To help with conventional commits, you can use our git message template:

```bash
git config commit.template .gitmessage
```

## Release Process

Our release process is fully automated:

1. **Development**: Work on the `main` branch
2. **Release**: Merge to `production` branch
3. **Automatic**: CI generates version, builds, and releases based on conventional commits

### Version Bumping

- **MAJOR** (1.0.0 → 2.0.0): Breaking changes (`feat!`, `fix!`, or `BREAKING CHANGE:`)
- **MINOR** (1.0.0 → 1.1.0): New features (`feat:`)
- **PATCH** (1.0.0 → 1.0.1): Bug fixes (`fix:`, `perf:`)

## Pull Request Guidelines

1. Use conventional commit format in your PR title
2. Provide a clear description of your changes
3. Include tests for new features
4. Update documentation if needed
5. Ensure all tests pass
6. Keep PRs focused on a single change

## Testing

Run tests before submitting your PR:

```bash
# Run all tests
go test ./...

# Run tests with race detection
go test -race ./...

# Build the project
make build
```

## Questions?

If you have questions about contributing, please open an issue or reach out to the maintainers.

Thank you for contributing to Elytra!
