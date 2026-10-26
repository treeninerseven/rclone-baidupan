---
title: "Baidu Netdisk"
description: "Experimental Baidu Netdisk backend using the XPan API"
---

# Baidu Netdisk

This experimental backend accesses your own Baidu Netdisk account using the
Open Platform XPan API. It supports listings, downloads, uploads, overwriting
files, directory creation, file deletion and removal of empty directories.
Public share links are not supported.

The implementation has HTTP mock tests. Authorization, paginated directory
listings, a small-file download and byte range reads have also been tested
against one real Baidu account. Token refresh and persistence across processes
have also been tested against Baidu by setting the local token expiry to the
past; natural expiry on Baidu's servers has not yet been tested. Actual access
depends on the permissions granted to your application and account. It does not
bypass Baidu's transfer limits. A backup with Chinese paths and an 8 MiB multipart
file has been uploaded and restored with matching SHA-256 checksums. Modification
times, same-size file updates and skipping unchanged files have been verified.

## Configuration

Create an application in the [Baidu Netdisk Open Platform](https://pan.baidu.com/union).
You need your application's AppKey and SecretKey. This backend uses authorization
code authentication with the `oob` redirect URI and the `basic,netdisk` scope;
confirm that your application permits this flow.

Run `rclone config`, create a remote, and enter `baidupan` as the storage type.
Enter the AppKey as `client_id` and the SecretKey as `client_secret`. Open the
authorization URL displayed by rclone in your browser, authorize the application,
and paste the resulting authorization code into rclone.

The secret is obscured in the configuration using rclone's password mechanism.
OAuth tokens are saved in the configuration and refreshed when necessary.
Use `rclone config reconnect baidu:` to authorize the remote again.

Protect your configuration file. Baidu's API passes credentials in query
parameters; HTTP dump logs can contain credentials and must be redacted before
sharing.

## Usage

With a remote named `baidu`, list the top level:

```sh
rclone lsf baidu:
```

List a subdirectory, including subdirectories recursively:

```sh
rclone lsf "baidu:我的文件" --recursive
```

Download a file:

```sh
rclone copyto "baidu:我的文件/example.txt" ./example.txt
```

Download a directory:

```sh
rclone copy "baidu:我的文件" ./downloads
```

Back up a local directory to your application's folder (replace `APP_NAME` with
your application's name):

```sh
rclone copy ./documents "baidu:apps/APP_NAME/backups/documents" --create-empty-src-dirs
```

`copy` adds and updates files while keeping destination files that are absent
locally. Use a dedicated backup directory. An overwrite replaces the previous
content at the same path; it does not create a dated backup version.

Restore and verify a backup by reading its contents:

```sh
rclone copy "baidu:apps/APP_NAME/backups/documents" ./restored
rclone check ./documents "baidu:apps/APP_NAME/backups/documents" --download
```

Uploads normally require a path under `/apps/APP_NAME/`. Permissions outside
that directory depend on your application and must not be assumed from the
ability to list or download files elsewhere.

## Uploads

XPan requires all part MD5 checksums before upload starts. The backend stages
each source file in a temporary file, computes its part hashes, sends the
requested parts, verifies their returned hashes and commits the complete file.
Only the requested remote path is accepted; an unexpected renamed destination
or wrong size is an error. Staged files are removed after success, failure or
cancellation. Use `--temp-dir` to select a disk with sufficient space. Concurrent
transfers require space for each staged source file.

The default part size is 4 MiB. `--baidupan-chunk-size` also accepts 16 MiB and
32 MiB, subject to Baidu account membership limits. The backend currently limits
uploads to 1024 parts: 4 GiB per file with the default chunk size. Baidu's own
file-size and account limits still apply. Parts upload sequentially within each
file; `--transfers` controls concurrent file uploads.

Modification times are supplied on upload with one-second precision and read
back from file metadata. Existing files cannot have their modification time
changed without reuploading them.

## Encrypted backups

Wrap a dedicated application directory in a [crypt remote](/crypt/) to encrypt
content and optionally file and directory names. For example, point a crypt
remote named `baiducrypt` at `baidu:apps/APP_NAME/encrypted-backups`, then upload
and restore through `baiducrypt:`. Preserve the crypt password and any custom
salt independently of the Baidu account credentials.

This combination has been tested against one real account with a multipart
file, Chinese and reserved names, an empty file and an empty directory.
Restored SHA-256 hashes, range decryption, content checks and skipping unchanged
files passed. Wrong-password and modified-ciphertext tests on locally downloaded
copies failed as expected. Observed Baidu upload staging files contained
ciphertext. Size limits apply to the encrypted size, including crypt overhead.

## Limitations

- Zero-byte files and empty directories are supported. Empty files use the
  empty-content MD5 and are committed without uploading data parts when the
  server requests none.
- Checksums are not advertised until the API's hash semantics are verified with
  arbitrary existing files. Use `rclone check --download` for a content check.
- Unknown-size uploads are rejected by the backend. Normal local-file backups
  have known sizes.
- Server-side copy, move, recursive deletion and cross-process upload resume are
  not implemented.
- Looking up a single file requires listing its parent directory, potentially
  across multiple pages.
- Each download obtains a fresh download link. Byte range responses are checked
  for HTTP 206, the requested Content-Range and, when provided, Content-Length.
  A server that ignores Range requests causes an error.
- The backend uses `pan.baidu.com` as its User-Agent, and upgrades HTTP download
  links to HTTPS. Redirects that downgrade HTTPS are rejected.
- Directory listings are not snapshots. Changes made during pagination can
  affect the returned entries.

<!--- autogenerated options start - DO NOT EDIT, instead edit fs.RegInfo in backend/baidupan/baidupan.go then run make backenddocs -->
<!--- autogenerated options stop -->
