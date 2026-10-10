# Restacking checked-out branches

Restack checks `git worktree list` before moving refs. If a branch to re-cut is
checked out in the main directory or another operator worktree, it stops before
any move and names that path. Switch that checkout to another branch or detach
HEAD, then run restack again. Clean managed task worktrees are refreshed with
their branch; dirty task worktrees block the operation. Ref updates carry a
`saddle:` reflog message so a later recovery can identify Saddle's moves.
