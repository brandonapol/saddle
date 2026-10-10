# Interrupted restack recovery

Restack computes the rebuilt tips before changing branches, saves a synced
`.saddle/restack-journal.json`, then updates the task and integration refs with
one `git update-ref --stdin` transaction using compare-and-swap old tips. It
detaches clean managed checkouts before the transaction, so an interrupted
refresh leaves their files matching their detached HEAD. SQLite ranges and
checkouts are refreshed afterwards; only then is the journal removed.

While a journal exists, train operations stop with recovery instructions and
`saddle doctor` reports it. `saddle restack --continue` finishes the saved ref,
range and checkout updates; `saddle restack --abort` restores the old tips and
ranges. Both handle interruption before or after the Git commit and retain any
uncommitted work. Abort preserves a ref changed externally rather than erasing
it; subsequent drift checks identify it. Run `saddle prs` after recovery to
retry remote publication. No hook removal or database editing is needed.
