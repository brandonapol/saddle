# audit: check a posting

Difficulty: mechanical.

Add `audit.Check`, which reads a posting through `ledger.View`. Stack this on
the ledger change: a red check on that layer holds this one.

After: #1

claims: audit/**
