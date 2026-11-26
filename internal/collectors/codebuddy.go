package collectors

// CodeBuddyCollector imports per-request usage from CodeBuddy transcripts.
type CodeBuddyCollector struct{}

func NewCodeBuddyCollector() *CodeBuddyCollector { return &CodeBuddyCollector{} }

func (c *CodeBuddyCollector) Info() Info {
	return (&buddyCollector{id: "codebuddy", name: "CodeBuddy"}).InfoWithDescription("Import per-request usage from CodeBuddy project and subagent transcripts")
}

func (c *CodeBuddyCollector) Collect(ctx Context, cursors []Cursor) (Result, error) {
	return (&buddyCollector{id: "codebuddy", name: "CodeBuddy"}).Collect(ctx, cursors)
}

var _ Collector = (*CodeBuddyCollector)(nil)
