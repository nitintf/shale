

# shale

A storage engine written from scratch in Go.

## What it is

An embedded key-value storage engine. A file of fixed-size pages, a buffer pool
in front of it, a B-tree for lookups, and a write-ahead log so a crash doesn't
lose committed data.

Single node, single process. No network, no SQL, no query planner.

## Where it's going

Four stages, each one usable on its own and building on the last: pages and the
buffer pool, then the B-tree, then write-ahead logging and crash recovery, then
an LSM engine behind the same API.

---

This project is based on my learnings from the Database Internals book.
