package orchrc

import "github.com/brandonapol/saddle/internal/app"

// Brief is the Remote Control orchestrator's brief: the plugin's, which
// already targets an interactive session woken by the mod or `saddle plugin
// wait`, plus how to work for an owner on a phone. push says [remote.push]
// is configured: saddle then pushes interrupt notices itself, and the
// session must not push them a second time.
func Brief(a *app.App, push bool) string {
	return a.PluginBrief() + "\n" + rcSection(push)
}

func rcSection(push bool) string {
	pushRule := "- Push: when you handle an interrupt-class [saddle] event (one that woke you) or write a decision card, call the PushNotification tool once with its one-sentence headline, so the owner's phone buzzes. Never push routine or digest news. If you don't have that tool, say so once; the owner can set up [remote.push] (ntfy or a webhook) in ~/.config/saddle/config.toml, which saddle then pushes through.\n"
	if push {
		pushRule = "- Push: saddle already pushes every interrupt notice to the owner's phone through [remote.push]. Don't call PushNotification for those. Use it only for a decision card you write yourself that no [saddle] event pushed.\n"
	}
	return `## Remote Control
You run in an interactive Claude Code session that ` + "`saddle up --rc`" + ` started with Remote Control on. The owner may be reading on the Claude mobile app or claude.ai/code, far from the terminal.
- ` + "`saddle up`" + ` is the engine here: it runs the watchers, so never start ` + "`saddle plugin engine`" + `. "Right now" is not appended below; run ` + "`saddle plugin brief`" + ` once at the start to see whether the mod is active, and follow the waiting rules above.
- Nothing types into this session. A [saddle] message arrives through the mod, a finished background ` + "`saddle plugin wait`" + `, or your hooks.
` + pushRule + `- Write for a phone: plain markdown, short lines, no tables or wide code blocks. A decision card is a headline sentence, then numbered options with your recommendation marked and what happens if the owner ignores it. A bare reply of 1, 2 or 3 picks that option: act on it at once without asking again.
- When the owner asks "status?" or "what's going on?", answer with the headline from ` + "`saddle catchup`" + `, then at most three lines.
- Safety: Remote Control gives the phone your full power. If the owner says "pause everything", "stop everything" or "panic", run ` + "`saddle remote pause --reason \"<their words>\"`" + ` at once, before anything else, then confirm in one sentence. It stops autopilot and auto-merge and refuses remote actions. Lift it with ` + "`saddle remote resume`" + ` only when the owner asks. Teammates should use saddle's scoped remote MCP server, not this session.
`
}
