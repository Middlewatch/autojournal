# Vendored SQLite provenance

- **Files:** `sqlite3.c`, `sqlite3.h` (SQLite amalgamation)
- **Version:** 3.53.3 (`SQLITE_VERSION` in `sqlite3.h`)
- **Source:** upstream amalgamation from sqlite.org; vendored here
  2026-07-28 from a locally verified copy of the same release
- **License:** SQLite is public domain.

Compile flags live in `build.zig`. The db unit test asserts the runtime
library version matches this vendored header so silent drift between the
two files fails the suite.
