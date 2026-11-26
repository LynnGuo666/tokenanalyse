package collectors

func BuiltinRegistry() *Registry {
	registry := NewRegistry()
	registry.Register(&ClaudeCodeCollector{})
	registry.Register(&CodexCollector{})
	registry.Register(&GeminiCliCollector{})
	registry.Register(&GrokBuildCollector{})
	registry.Register(&OpenCodeCollector{})
	registry.Register(&PiCollector{})
	registry.Register(&OMPCollector{})
	registry.Register(&KimiCollector{})
	registry.Register(&CopilotCollector{})
	registry.Register(&CodeBuddyCollector{})
	registry.Register(&WorkBuddyCollector{})
	registry.Register(&DSHCollector{})
	registry.Register(&MCodeCollector{})
	registry.Register(&AntigravityCollector{})
	return registry
}
