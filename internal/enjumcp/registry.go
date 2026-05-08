// Package enjumcp owns the canonical list of every Enju MCP
// tool's schema (name, description, input params).
//
// Today the fat-client is the only consumer — it iterates this
// registry to wire one handler per tool. The dispatcher's
// New() validates that registered handler names match registry
// entries (loud-and-early panic on mismatch).
//
// Schemas are pure data with no I/O dependency. Both sides could
// read this if a hosted-mode MCP transport ever ships; today
// only fat-client imports it. The boundary check
// (./build.sh check-imports) prevents either side from accidentally
// becoming a writer.
package enjumcp

import "github.com/mark3labs/mcp-go/mcp"

// Registry is the canonical list of every Enju MCP tool. Order
// is not load-bearing — iteration is fine in any order. Add a
// new tool by adding one entry here and writing a handler in
// internal/fatclient/mcphandlers/.
var Registry = []mcp.Tool{
	ListRuns(),
	ListReadyTasks(),
	ReleaseTask(),
	RunStatus(),
	PauseRun(),
	ResumeRun(),
	TerminateRun(),
	SpawnTask(),
	RequestClarification(),
	SetCycleBudget(),
	ShowEvents(),
	RecentEvents(),
	EventsStatus(),
	ListIterations(),
	FileIssue(),
	ListIssues(),
	GetIssue(),
	TriageIssue(),
	CloseIssue(),
	MyDashboard(),
	UpdateProfile(),
	ListProjects(),
	SetProjectDefaultBranch(),
	LeaveProject(),
	AddProjectMember(),
	RemoveProjectMember(),
	ListProjectMembers(),
	PromoteMember(),
	DemoteOwner(),
	ListArtifacts(),
	MyProfile(),
	InvalidateTask(),
	TallyTask(),
	FailTask(),
	ExecuteTask(),
	RegisterBot(),
	ListMyBots(),
	RevokeToken(),
	ListModels(),
	RegisterModel(),
	BotStart(),
	BotStop(),
	BotStatus(),
	BotLogs(),
	BotStartAll(),
	BotStopAll(),
	ClaimTask(),
	ClaimReadyMatching(),
	GetTaskInputs(),
	SubmitResult(),
	SubmitResultsBatch(),
	GetTask(),
	CreateRun(),
	ExecuteRun(),
	ExportRun(),
	ExportDiagram(),
	ExportRunEvents(),
	ListTemplates(),
	DescribeTemplate(),
	GetArtifact(),
	GetArtifactHistory(),
	ListUntrackedArtifacts(),
	Inbox(),
	Review(),
	CreateProject(),
	SetProjectRemote(),
	ProjectRemoteStatus(),
	ProjectSync(),
}

// All returns every tool in Registry. Useful for the fat-client's
// Register loop ("for every tool, ensure a handler is wired").
func All() []mcp.Tool {
	out := make([]mcp.Tool, len(Registry))
	copy(out, Registry)
	return out
}

// ByName looks up a tool by its MCP tool name (e.g.
// "enju_list_runs"). Returns (zero, false) when not found.
// Used by the dispatcher to validate handler registrations.
func ByName(name string) (mcp.Tool, bool) {
	for _, t := range Registry {
		if t.Name == name {
			return t, true
		}
	}
	return mcp.Tool{}, false
}
