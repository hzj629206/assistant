package codex

// Codex is the main client for interacting with the Codex CLI.
// Use StartThread to begin a new thread or ResumeThread to continue an existing one.
type Codex struct {
	exec    *CodexExec
	options CodexOptions
}

// NewCodex constructs a Codex client with optional settings.
func NewCodex(options CodexOptions) *Codex {
	return &Codex{
		exec:    NewCodexExec(options.CodexPathOverride, options.Env),
		options: options,
	}
}

// StartThread begins a new conversation with the agent.
func (c *Codex) StartThread(options ThreadOptions) *Thread {
	return NewThread(c.exec, c.options, options, "")
}

// ResumeThread continues a conversation using an existing thread ID.
func (c *Codex) ResumeThread(id string, options ThreadOptions) *Thread {
	return NewThread(c.exec, c.options, options, id)
}
