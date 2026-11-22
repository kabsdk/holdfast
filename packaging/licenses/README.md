# Third-party licences

The licence files of software in Holdfast's packages that the Go module cache does not hold (decision 043).
`scripts/third-party-notices.sh` reads them; each Go module's own licence files are read from the module cache at
build time.

| File | Of | From | sha256 |
| --- | --- | --- | --- |
| `go.LICENSE` | the Go standard library, linked into every program | `https://raw.githubusercontent.com/golang/go/go1.26.8/LICENSE` | `911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad` |
| `go.PATENTS` | its patent grant | `https://raw.githubusercontent.com/golang/go/go1.26.8/PATENTS` | `96f408bfae65bf137fc2525d3ecb030271c50c1e90799f87abf8846d8dd505cc` |
| `htm-3.1.1.LICENSE` | htm 3.1.1, in the UI's vendored module | `https://unpkg.com/htm@3.1.1/LICENSE` | `740725f7252e750af735d0028cc534970772f513331e9f68150fede8fb3ce00f` |
| `preact-10.LICENSE` | Preact 10, bundled in that module | `https://unpkg.com/preact@10.2.0/LICENSE` | `1fe6958409c8c257a70c587a18b6f7f412b179b456630790d30b2ec9a8e4b7d4` |
| `restic.LICENSE` | restic 0.19.1, in `holdfast-restic` | `https://raw.githubusercontent.com/restic/restic/v0.19.1/LICENSE` | `6f08a01a9fab5b24e139a09f15cc24a73087c7bc09e3bacf099fdf2d767bf897` |

When Go, restic or the UI's vendored module is updated, fetch its licence again from the new version and change this
table.
