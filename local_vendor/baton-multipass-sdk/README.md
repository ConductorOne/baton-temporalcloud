# baton-multipass-sdk (temporary local copy)

Temporary, trimmed copy of `github.com/conductorone/baton-multipass-sdk`
(`pkg/multipass`) at v0.0.1, commit b9f4b6776da475759bbcdc18f8209fedab389a6c.
The upstream module is private; this copy exists until it is public (IGA-5320).

Do not edit this code to diverge from upstream.

This directory is an ordinary package of the connector module (no nested
`go.mod`, no `require`, no `replace`), imported as
`github.com/conductorone/baton-temporalcloud/local_vendor/baton-multipass-sdk/pkg/multipass`.

To switch to the public module: add it to the root `go.mod`, change the import
path, delete this directory, then run `go mod tidy && go mod vendor`.
