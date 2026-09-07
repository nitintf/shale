# shale

A storage engine written from scratch in Go, to learn how databases actually work.

I'm reading Database Internals (Alex Petrov) and building the thing each chapter
describes instead of only taking notes on it. Reading about a page split and
implementing one turn out to be very different amounts of understanding.

## What it is

An embedded key-value storage engine. A file of fixed-size pages, a buffer pool
in front of it, a B-tree for lookups, and a write-ahead log so a crash doesn't
lose committed data.

Single node, single process. No network, no SQL, no query planner.

## The plan

Four stages. Each one works on its own and the next one builds on it.

### Stage 0: pages and the buffer pool

The substrate. Chapter 1 of the book.

- Fixed-size pages with a slotted layout: header, slot array growing down,
  records growing up from the back.
- A pager: allocate a page, read page `N`, write page `N`. Page `N` lives at
  byte `N * pageSize`.
- A buffer pool: a fixed set of frames, a hash table from page number to frame,
  pin counts, dirty flags, clock-sweep eviction.
- Insert, get and delete a record by `(page, slot)`. Full scan.

Done when records survive a close and reopen, and a buffer pool far smaller than
the file still works.

### Stage 1: the B-tree

Chapters 2 to 4.

- Internal pages that route and leaf pages that hold `key -> (page, slot)`.
- Descent by binary search inside each page.
- Page splits, cascading up to the root.
- Sibling pointers on the leaves, so range scans walk sideways.

Done when 100k random keys insert and read back, and ordered iteration returns
them sorted.

### Stage 2: write-ahead log and recovery

Chapter 5.

- Log records, the write-ahead rule (the log reaches disk before the page does),
  checkpoints, redo on open.

Done when I can `kill -9` the process in a loop and every committed record is
still there afterwards.

### Stage 3: an LSM engine

Chapter 7.

- Memtable, SSTable flush, compaction, bloom filters.
- Same public API, a second engine behind it.

Done when I can benchmark B-tree against LSM on the same workload and explain
the difference from the numbers.

## Rules

- **No third-party dependencies.** `os`, `encoding/binary`, `sync`,
  `hash/crc32`. A storage engine with a full `go.sum` has outsourced the
  interesting parts.
- **Tests from the first commit.** `make test` runs with `-race`.
- **Correctness before speed.** Benchmarks come after it's right.

## Status

Stage 0, in progress.
