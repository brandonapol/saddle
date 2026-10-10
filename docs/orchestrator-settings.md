# Repairing orchestrator permissions

`saddle doctor --fix` merges `Bash(saddle:*)` and `mcp__saddle` into
`.claude/settings.local.json`, preserving other settings and permission rules.
Malformed JSON is reported with its path and left untouched. Doctor also warns
about single-underscore `mcp_saddle...` permission rules and Saddle tool names
in `enabledMcpjsonServers`; that list takes the server name `saddle`.
