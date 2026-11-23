# DokuWiki filesystem source

This connector reads `pages/**/*.txt` and supported files below `media/` from
an administrator-approved DokuWiki data mount. It never writes to the mount,
uses wiki credentials, reads a user database, or fetches content from the public
wiki URL. Deploy the source volume as read-only.

The deployment variable `WEKNORA_DOKUWIKI_SOURCES` is a JSON alias map. The UI
sends only an alias and selection; it cannot set host paths or source URLs.

```json
{
  "vendor-wiki": {
    "data_root": "/wiki-vendor-data",
    "public_base_url": "https://wiki.example.test/wiki",
    "sentinel": "pages/release/start.txt",
    "allowed_namespaces": [""],
    "denied_namespaces": ["restricted"]
  }
}
```

Set optional `acl_path` to a separately mounted read-only `acl.auth.php` file
and `expected_acl_sha256` to its reviewed SHA-256. Both fields must be supplied
together. Validation, resource listing, scans and publication verification stop
if the fingerprint changes. The file is fingerprinted only; the connector does
not parse ACL rules or grant knowledge-base membership. Mount just that file,
not the wiki's entire configuration or identity directory. A changed server
source binding/policy also rejects an existing cursor until publications have
been reviewed and the cursor deliberately reset.

A data-source config has this shape (there are no credentials):

```json
{
  "type": "dokuwiki_filesystem",
  "settings": {
    "source_alias": "vendor-wiki",
    "file_types": ["txt", "pdf", "xlsx", "xls", "docx"]
  },
  "resource_ids": ["namespace:"]
}
```

`namespace:` selects the entire allowed source; `namespace:release:chips`
selects that namespace and descendants across pages and media. Empty resource
selection is rejected when syncing. The resource tree contains namespaces,
not host paths. Missing `file_types` defaults to all five supported formats;
an empty list is rejected. Original file names, bytes, source URL, path,
namespace and SHA-256 are retained. Source IDs distinguish wiki pages from
media and aliases from each other; moving/renaming a file creates a new source
identity. Duplicate bytes at different paths are not merged across permissions.

The production path streams one file at a time. Unchanged supported content is
hashed but is not emitted or reparsed. A complete detached cursor is saved
after each successful ingestion. A confirmed document-content rejection is
recorded in the cursor's `failed` map, keeps the previous successful hash and
allows later files to continue; it is retried on the next scan. Infrastructure,
authorization, configuration and publication errors abort the scan. Neither
kind acknowledges the failed file. The optional `VerifyFetchedItem` method rechecks
the original bytes and source/ACL policy after asynchronous parsing and before
publication. The scanner refuses path traversal, symbolic links, non-regular
files, missing/changed mounts and missing/empty sentinels. A second metadata
inventory must match before a scan is marked complete.

Pure navigation/download-list pages, empty pages and image-only pages are
excluded with reasons. Code and tables remain body candidates. ZIP, images,
audio and other unsupported files are listed in the cursor's `excluded` map
but never counted as parsed body documents. `excluded_counts` also records
policy exclusions without revealing denied file names. No OCR, archive
extraction, spreadsheet conversion or inference of missing content is done.
Supported extensions are candidates, not a promise that the parser accepts
every source file. Oversize or unstable candidates fail the run explicitly.

Deletion sync is deliberately unsupported: this type advertises no
`deletion_sync` capability, and its data sources must keep `sync_deletions`
false. Missing files, changed scope/format selections and moved files do not
automatically retire prior published documents. Review and remove obsolete
publications explicitly until a separately tested deletion lifecycle exists.
An unchanged connector cursor also presumes that administrators have not
manually removed its corresponding knowledge documents; use a full sync to
rebuild those associations if needed.

Run semantic connector tests with:

```sh
go test ./internal/datasource/connector/dokuwiki
```

The tests verify original bytes and hashes, namespace policy and traversal
rejection, exclusion accounting, stable incremental identity, ingestion failure
resume, mount/symlink failures, changing scans and ACL publication guards.
They do not establish parser quality, whole-library throughput, live member
authorization or UI behavior.
