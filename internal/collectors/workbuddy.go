package collectors

// WorkBuddyCollector imports OpenAI-style request usage from WorkBuddy transcripts.
type WorkBuddyCollector struct{}

func NewWorkBuddyCollector() *WorkBuddyCollector { return &WorkBuddyCollector{} }

func (c *WorkBuddyCollector) Info() Info {
	return (&buddyCollector{id: "workbuddy", name: "WorkBuddy"}).InfoWithDescription("Import OpenAI-style request usage from WorkBuddy project and subagent transcripts")
}

func (c *WorkBuddyCollector) Collect(ctx Context, cursors []Cursor) (Result, error) {
	return (&buddyCollector{id: "workbuddy", name: "WorkBuddy"}).Collect(ctx, cursors)
}

var _ Collector = (*WorkBuddyCollector)(nil)
