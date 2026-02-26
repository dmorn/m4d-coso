package agent

import (
	"encoding/json"
	"fmt"
	"github.com/dmorn/m4dtimes/sdk/llm"
)

type registeredTool struct {
	def     llm.ToolDef
	handler ToolHandler
}

type ToolRegistry struct {
	tools map[string]registeredTool
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: map[string]registeredTool{}}
}

// RegisterTool registers a Tool implementation.
// Equivalent to Register with the tool's Def() and Execute() method.
func (r *ToolRegistry) RegisterTool(t Tool) {
	def := t.Def()
	r.Register(def.Name, def.Description, def.Parameters, t.Execute)
}

// RegisterToolSet registers all tools from a ToolSet.
func (r *ToolRegistry) RegisterToolSet(ts ToolSet) {
	for _, t := range ts.Tools() {
		r.RegisterTool(t)
	}
}

func (r *ToolRegistry) Register(name, description string, schema json.RawMessage, handler ToolHandler) {
	if r == nil {
		return
	}
	r.tools[name] = registeredTool{
		def: llm.ToolDef{
			Name:        name,
			Description: description,
			Parameters:  schema,
		},
		handler: handler,
	}
}

// Execute runs the handler for the given tool call.
// Returns a ToolResult — errors are captured as IsError:true, never panics.
func (r *ToolRegistry) Execute(name string, args json.RawMessage, ctx ToolContext) *llm.ToolResult {
	if r == nil {
		return &llm.ToolResult{Content: "tool registry is nil", IsError: true}
	}
	tool, ok := r.tools[name]
	if !ok {
		return &llm.ToolResult{Content: fmt.Sprintf("unknown tool: %s", name), IsError: true}
	}
	if tool.handler == nil {
		return &llm.ToolResult{Content: fmt.Sprintf("tool has no handler: %s", name), IsError: true}
	}

	result, err := tool.handler(ctx, args)
	if err != nil {
		return &llm.ToolResult{Content: err.Error(), IsError: true}
	}
	return &llm.ToolResult{Content: result, IsError: false}
}

// Definitions returns []llm.ToolDef for passing to the LLM.
func (r *ToolRegistry) Definitions() []llm.ToolDef {
	if r == nil {
		return nil
	}
	out := make([]llm.ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t.def)
	}
	return out
}
