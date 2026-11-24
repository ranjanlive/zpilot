# Releasing zagent

For maintainers. A release is six archives plus a checksum file, produced by one script.

## Before the first public release

- [ ] **Choose and add a `LICENSE`** (the packaging scripts include it automatically when the file exists). The repository does not ship one yet, and publishing without it leaves the code all-rights-reserved.
- [ ] Add a `SECURITY.md` with a private contact (referenced from `CONTRIBUTING.md`).
- [ ] Decide the repository URL and replace the `<repo>` placeholder in `docs/DEVELOPMENT.md`; add the releases link to `docs/INSTALL.md`.
- [ ] Consider code-signing: the Windows `.exe` is unsigned (SmartScreen will warn) and the macOS binary is not notarised (users need `xattr -d com.apple.quarantine zagent`, as documented in `INSTALL.md`).

## Cutting a release

1. Update `CHANGELOG.md`: rename *Unreleased* to the version and date; start a new *Unreleased*.
2. `make check` on a clean checkout, and `GOOS=linux go vet ./...`, `GOOS=darwin go vet ./...`.
3. Run the opt-in live tests against a real endpoint (`go test -tags live -run Live -v ./internal/provider/`; see the file header for the environment variables) when provider code changed.
4. Smoke-test the real binary: `zagent init --home /tmp/h`, fill `.env`, `zagent doctor --home /tmp/h`, a short `zagent -p` task, `--ui web`.
5. Tag: `git tag v1.2.3 && git push --tags`.
6. Build the archives:
   ```powershell
   .\scripts\package.ps1 -Version 1.2.3          # Windows (also builds every other platform)
   ```
   ```sh
   VERSION=1.2.3 ./scripts/package.sh            # Linux / macOS (needs tar; zip for the Windows archives)
   ```
   Output in `dist/`:
   ```
   zagent-1.2.3-windows-amd64.zip   zagent-1.2.3-windows-arm64.zip
   zagent-1.2.3-linux-amd64.tar.gz  zagent-1.2.3-linux-arm64.tar.gz
   zagent-1.2.3-darwin-amd64.tar.gz zagent-1.2.3-darwin-arm64.tar.gz
   SHA256SUMS.txt
   ```
   Each archive holds the binary, `README.md`, `CHANGELOG.md`, `docs/`, `examples/` (and `LICENSE`). Binaries are static (`CGO_ENABLED=0`), stripped, built with `-trimpath`, and stamped with the version (`zagent version`).
7. Create the release on the hosting site, upload all seven files, and paste the changelog section as the notes.
8. Download one archive from the release page, verify it against `SHA256SUMS.txt`, and run through `docs/INSTALL.md` once as a new user would.

## Versioning

[SemVer](https://semver.org). Treat as the public interface: the CLI flags and exit codes, `.env` and `config.json` keys, the tool manifest format, the control API, and the session log format. Additive changes are minor releases; renaming or removing any of them, or changing the meaning of an existing key, is a major release. The session log has no version field yet; if its format must ever change incompatibly, add one first and migrate on open.

## Compatibility notes to carry forward

- Users keep their home (config, sessions, memory, skills, tools) across upgrades; the engine never rewrites those files. New config keys must have defaults that preserve existing behaviour.
- A new engine reading an old session must keep working (it only ever appends); an old engine may not understand new event kinds.
