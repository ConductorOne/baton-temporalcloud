# baton-multipass-sdk (temporary local copy)

Temporary, trimmed copy of `github.com/conductorone/baton-multipass-sdk`
(`pkg/multipass`) at v0.0.1, commit b9f4b6776da475759bbcdc18f8209fedab389a6c.
The upstream module is private; this copy exists until it is public (IGA-5320).

Do not edit this code to diverge from upstream.

To swap in the public module: delete the `replace` line for
`github.com/conductorone/baton-multipass-sdk` in the root `go.mod`, delete this
`local_vendor/baton-multipass-sdk` directory, bump the `require` version, then
run `go mod tidy && go mod vendor`.
