# Security model

Gluestick's **core** is a Windows package manager library compatible with
[Scoop](https://github.com/ScoopInstaller/Scoop) buckets. This document
describes trust boundaries and known risks.

## Trust boundaries

### Buckets are code

Adding a bucket clones a Git repository. Bucket manifests may include:

- **Download URLs** pointing to third-party hosts
- **PowerShell hooks** (`pre_install`, `post_install`, `installer`, etc.) executed during install/uninstall
- **Shortcuts, persist, and env** directives that modify the user profile

**Treat buckets like running untrusted scripts.** Only add buckets from sources
you trust, and review manifest changes on bucket update (same model as Scoop).

### Manifest JSON overrides

`~/.glue/config.json` may contain a `manifests` map that replaces or patches
manifests locally. This is equivalent to **injecting arbitrary install
definitions** for matching package names. Anyone with write access to your data
directory can change install behavior.

### Downloads

Packages are fetched over HTTP(S)/FTP from URLs declared in manifests. core
verifies hashes when the manifest provides them; missing or weak hashes reduce
integrity guarantees.

### Local data directory

Default root: `~/.glue` (override via `EngineConfig.RootDir`). Protect this
directory with normal OS file permissions. It contains CAS blobs, cloned
buckets, SQLite indexes, and install trees.

## Path safety (install)

As of the current release line, **core** rejects manifest-relative paths
containing `..` or absolute segments for:

- `extract_to` / `extract_dir` layout
- Hardlink targets when reinstalling from cache or archive member indexes
- Adopted extract file paths indexed into the cache store

7-Zip extraction still writes under a destination directory chosen by the
engine; manifests that rely on malicious archives should be treated as untrusted
input. Report bypasses responsibly.

## Typed errors

Embedders can branch on stable errors via `errors.Is`:

| Sentinel | Meaning |
| --- | --- |
| `engine.ErrManifestNotFound` | No manifest for the reference |
| `engine.ErrManifestAmbiguous` | Same name in multiple buckets |
| `engine.ErrBucketNotInstalled` | Required bucket not present locally |
| `engine.ErrPackageNotInstalled` | Operation on a package that is not installed |

Resolve/manifest hints (including "Did you mean") are classified with
`engine.IsInstallResolveNotice` without string matching.

## Reporting issues

Security-sensitive bugs: open a GitHub Security Advisory on this repository.
Include a reproduction bucket/manifest and the core version.
