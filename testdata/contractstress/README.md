# Contract stress fixtures

All data in this directory is synthetic. `generate_sqlite_fixtures.py`
recreates the checked-in SQLite and WAL bytes from its fixed base64 payloads.
Run it from this directory with `python3 generate_sqlite_fixtures.py` after
changing those payloads, then update the SHA-256 values in
`opencode-manifest.json`.

The semantic manifest is the test-only representation used by the contract
stress package. The SQLite and WAL files are evidence fixtures only; the
production module intentionally has no SQLite dependency.
