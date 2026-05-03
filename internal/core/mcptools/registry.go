// Package mcptools is the single source of truth for every MCP
// tool Enju exposes: name, description, input schema, and which
// side of the architecture (coordinator vs fat-client) owns the
// handler.
//
// Both coordinator and fat-client iterate this registry to wire
// up their MCP server. New tools land here once; the side
// classification flows everywhere else automatically.
//
// Why this lives in core: schema metadata is pure data with no
// I/O dependency. Both sides need to read it; neither side owns
// it. The boundary check (tools/check-imports.sh) prevents
// either side from accidentally becoming the source of truth.
package mcptools

import "github.com/mark3labs/mcp-go/mcp"

// Side is the architectural side that owns a tool's handler.
type Side int

const (
	// SideCoordinator: the handler lives in coordinator code
	// and runs natively when the coordinator serves MCP. The
	// fat-client also re-exposes these by forwarding the call
	// over HTTP.
	SideCoordinator Side = iota

	// SideFatClient: the handler lives in fat-client code and
	// runs locally. Touches git, live.jsonl, supervisor, or
	// other fat-client-only state. Coordinator does NOT expose
	// these — a hosted-only deployment without a local fat-
	// client cannot perform write operations.
	SideFatClient
)

// Tool bundles a tool's MCP schema with its side classification.
type Tool struct {
	// Tool is the mcp-go schema (name, description, input
	// params) the agent sees.
	Tool mcp.Tool
	// Side declares who owns the handler. Determines whether
	// the tool gets registered natively coord-side or
	// forwarded from fat-client.
	Side Side
}

// Registry is the canonical list of every Enju MCP tool. Order
// is not load-bearing — iteration is fine in any order. Add a
// new tool by adding one entry here and writing a handler on
// the appropriate side.
//
// Classification source: see docs/refactor-phase3-plan.md §3.1
// audit + the per-handler analysis in the same doc. Re-classify
// when a tool's handler grows or sheds local-state dependencies.
var Registry = []Tool{
	// --- Coordinator-side: thin handlers, just talk to state.db / events.db ---
	{Tool: ListRuns(), Side: SideCoordinator},
	{Tool: ListReadyTasks(), Side: SideCoordinator},
	{Tool: ReleaseTask(), Side: SideCoordinator},
	{Tool: RunStatus(), Side: SideCoordinator},
	{Tool: PauseRun(), Side: SideCoordinator},
	{Tool: ResumeRun(), Side: SideCoordinator},
	{Tool: SpawnTask(), Side: SideCoordinator},
	{Tool: RequestClarification(), Side: SideCoordinator},
	{Tool: SetCycleBudget(), Side: SideCoordinator},
	{Tool: ShowEvents(), Side: SideCoordinator},
	{Tool: RecentEvents(), Side: SideCoordinator},
	{Tool: EventsStatus(), Side: SideCoordinator},
	{Tool: ListIterations(), Side: SideCoordinator},
	{Tool: FileIssue(), Side: SideCoordinator},
	{Tool: ListIssues(), Side: SideCoordinator},
	{Tool: GetIssue(), Side: SideCoordinator},
	{Tool: TriageIssue(), Side: SideCoordinator},
	{Tool: CloseIssue(), Side: SideCoordinator},
	{Tool: MyDashboard(), Side: SideCoordinator},
	{Tool: UpdateProfile(), Side: SideCoordinator},
	{Tool: ListProjects(), Side: SideCoordinator},
	{Tool: SetProjectDefaultBranch(), Side: SideCoordinator},
	{Tool: LeaveProject(), Side: SideCoordinator},
	{Tool: AddProjectMember(), Side: SideCoordinator},
	{Tool: RemoveProjectMember(), Side: SideCoordinator},
	{Tool: ListProjectMembers(), Side: SideCoordinator},
	{Tool: PromoteMember(), Side: SideCoordinator},
	{Tool: DemoteOwner(), Side: SideCoordinator},
	{Tool: ListArtifacts(), Side: SideCoordinator},
	{Tool: MyProfile(), Side: SideCoordinator},
	{Tool: InvalidateTask(), Side: SideCoordinator},
	{Tool: TallyTask(), Side: SideCoordinator},
	{Tool: FailTask(), Side: SideCoordinator},
	{Tool: ExecuteTask(), Side: SideCoordinator},
	{Tool: RegisterBot(), Side: SideCoordinator},
	{Tool: ListMyBots(), Side: SideCoordinator},
	{Tool: RevokeToken(), Side: SideCoordinator},
	{Tool: ListModels(), Side: SideCoordinator},
	{Tool: RegisterModel(), Side: SideCoordinator},

	// --- Fat-client-side: heavy handlers, touch git/workspace/supervisor ---
	{Tool: ClaimTask(), Side: SideFatClient},
	{Tool: ClaimReadyMatching(), Side: SideFatClient},
	{Tool: GetTaskInputs(), Side: SideFatClient},
	{Tool: SubmitResult(), Side: SideFatClient},
	{Tool: SubmitResultsBatch(), Side: SideFatClient},
	{Tool: GetTask(), Side: SideFatClient},
	{Tool: CreateRun(), Side: SideFatClient},
	{Tool: ExecuteRun(), Side: SideFatClient},
	{Tool: ExportRun(), Side: SideFatClient},
	{Tool: ExportDiagram(), Side: SideFatClient},
	{Tool: ExportRunEvents(), Side: SideFatClient},
	{Tool: ListTemplates(), Side: SideFatClient},
	{Tool: DescribeTemplate(), Side: SideFatClient},
	{Tool: GetArtifact(), Side: SideFatClient},
	{Tool: GetArtifactHistory(), Side: SideFatClient},
	{Tool: ListUntrackedArtifacts(), Side: SideFatClient},
	{Tool: Notifications(), Side: SideFatClient},
	{Tool: Inbox(), Side: SideFatClient},
	{Tool: Review(), Side: SideFatClient},
	{Tool: CreateProject(), Side: SideFatClient},
	{Tool: Init(), Side: SideFatClient},
	{Tool: SetProjectRemote(), Side: SideFatClient},
	{Tool: ProjectRemoteStatus(), Side: SideFatClient},
	{Tool: ProjectSync(), Side: SideFatClient},
}

// Coordinator returns the subset of Registry whose handlers live
// on the coordinator side. The coordinator's MCP server iterates
// this to register native handlers; the fat-client's MCP server
// iterates the same slice to register HTTP-forwarders.
func Coordinator() []Tool {
	return filterBySide(SideCoordinator)
}

// FatClient returns the subset of Registry whose handlers live
// on the fat-client side. Only the fat-client's MCP server
// iterates this — the coordinator never exposes these tools.
func FatClient() []Tool {
	return filterBySide(SideFatClient)
}

// All returns every tool in Registry, regardless of side.
// Useful for documentation generators and tool-list audits.
func All() []Tool {
	out := make([]Tool, len(Registry))
	copy(out, Registry)
	return out
}

// MustByName looks up a tool by its MCP tool name (e.g.
// "enju_list_runs"). Panics if missing — calls into this from
// production code reference a name that's expected to exist;
// a missing entry means a programmer error in the registry,
// not a runtime condition. Tests can use ByName to introspect
// without panicking.
func MustByName(name string) Tool {
	t, ok := ByName(name)
	if !ok {
		panic("mcptools: unknown tool name: " + name)
	}
	return t
}

// ByName looks up a tool by its MCP tool name. Returns
// (zero, false) when not found.
func ByName(name string) (Tool, bool) {
	for _, t := range Registry {
		if t.Tool.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

func filterBySide(s Side) []Tool {
	var out []Tool
	for _, t := range Registry {
		if t.Side == s {
			out = append(out, t)
		}
	}
	return out
}
