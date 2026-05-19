# Credentials & Identity

This guide covers how Enju recognizes you, where credentials are stored, how to connect to a coordinator, and how to manage identity in multi-user projects.

---

## How identity works

Every person and agent that acts in Enju is a **citizen** — a registered identity with a username, display name, and bearer token. The token is your credential: it authenticates every API call to the coordinator.

On first run, Enju registers you automatically using your git global config:

```sh
git config --global user.name  "Tamer Gur"
git config --global user.email "tamer@example.com"
enju start
```

Output:
```
Registered as @tamer (Tamer Gur)
```

The username is slugified from your display name (`"Tamer Gur"` → `tamer`). If `tamer` is already taken, the coordinator tries `tamer-2`, `tamer-3`, etc. You can override with an explicit username:

```sh
enju mcp --username tamerh
```

**Email is the globally-unique key for human citizens.** Name is required for display. Both must be set in git global config for auto-registration to succeed — a partial config produces an actionable error message rather than a 400 from the coordinator.

---

## The credentials file

Registration writes `~/.enju/credentials.json`:

```json
{
  "coordinator": "http://localhost:8333",
  "username": "tamer",
  "name": "Tamer Gur",
  "email": "tamer@example.com",
  "token": "..."
}
```

**This file is your key.** It is written with `0600` permissions (owner-read only). Protect it like a password.

On subsequent runs, Enju loads this file and skips registration. The coordinator URL is also read from this file as the default for `--coordinator`, so you don't need to pass it on every invocation.

---

## Connecting to a coordinator

By default, Enju connects to `http://localhost:8333` — the pinned port used by `enju start`. Override with `--coordinator`:

```sh
enju mcp --coordinator http://team-server:8333
```

Any tool that contacts the coordinator accepts `--coordinator`:

```sh
enju go enju.yaml --coordinator http://team-server:8333
enju agent setup --coordinator http://team-server:8333
```

Once you've connected to a remote coordinator, the URL is saved in `credentials.json` and becomes the default for future invocations. You only need `--coordinator` on the first call to a new server.

---

## Multiple identities on one machine

`credentials.json` is keyed by coordinator URL, so one file holds parallel identities cleanly — your local instance, a team server, a separate research project:

```json
{
  "coordinator": "http://localhost:8333",
  "username": "tamer",
  "token": "..."
}
```

When you run against a different coordinator, the file gains a second entry under that URL.

**Running two MCP processes for different citizens on the same host** — use `--credentials` to give each process its own file:

```sh
# Alice's MCP process
enju mcp --coordinator http://team-server:8333 --credentials ~/.enju/alice.json

# Bob's MCP process
enju mcp --coordinator http://team-server:8333 --credentials ~/.enju/bob.json
```

Each process loads and saves credentials from its own file, without touching the other's identity.

---

## Project membership

Every project-scoped action — reading tasks, claiming work, submitting results — requires project membership. The citizen who creates the project is automatically the owner. Everyone else must be added explicitly.

**Membership tiers:**

| Role | Capabilities |
|------|-------------|
| `owner` | Everything members can do, plus: add/remove members, promote/demote owners, change remote URL |
| `member` | Read project state, claim tasks, submit results, add other members |

**Managing membership via MCP:**

```
enju_add_project_member project_id=3 username=alice
enju_list_project_members project_id=3
enju_promote_member project_id=3 username=alice
enju_remove_project_member project_id=3 username=alice
enju_leave_project project_id=3
```

**One invariant:** every project must have at least one owner at all times. Promote a successor before demoting yourself or the last owner.

---

## Agent credentials

Agents get their own credentials file at the path declared in the workflow manifest. This is separate from your human credentials:

```
~/.enju/credentials.json          ← your human identity
~/.enju/credentials/dev-agent.json  ← dev-agent's identity (example path)
```

`enju agent setup` registers each agent and writes its credentials file. The operation is idempotent — running it again on a project where agents are already registered skips them silently.

If an agent's credentials file is lost, re-run `enju agent setup` to rotate the token and write a fresh file:

```sh
enju agent setup --project-id=3
```

The old token is revoked and replaced. Any in-flight claims held by the agent will time out and become re-claimable.

---

## Token rotation

**For agents:** use `enju agent rotate-token`:

```sh
enju agent rotate-token --agent=developer-agent
```

This revokes all active tokens for the agent and mints a fresh one. The new credentials are written to the agent's manifest path immediately.

**For your own token:** delete `~/.enju/credentials.json` and re-run `enju start`. Because registration is idempotent by email — re-registering with the same email returns the same citizen with a fresh token — your history, project membership, and submitted work are all preserved.

---

## Team deployment

In a team setup, the coordinator runs on a shared server. Each team member runs `enju mcp` (or `enju start`) with `--coordinator` pointing at it — that is the only configuration change.

```sh
# On the server
enju serve --db /var/enju/state.db --port 8333

# On each team member's machine
enju mcp --coordinator http://your-server:8333
```

Git operations still happen locally on each member's machine. The coordinator handles authentication and state; git holds the file content and history.

Each team member registers once against the shared coordinator and gets their own bearer token. Project owners add members via `enju_add_project_member` before those members can claim tasks.

---

## Security notes

- **Tokens don't expire.** Protect `credentials.json` accordingly — treat it like an SSH private key.
- **No password or OAuth.** The token *is* the credential. There is no recovery flow for a lost token beyond re-registering (which rotates to a fresh token by email lookup).
- **No email verification.** Email is used as the globally-unique key for identity deduplication, not for sending messages or logging in.
- **Agent tokens are separate.** A compromised agent credentials file cannot be used as your human identity. Rotate agent tokens independently via `enju agent rotate-token`.

---

## Next steps

- [Agents](agents.md) — agent registration, daemon lifecycle, token rotation in workflow context
- [Writing Workflows](writing-workflows.md) — `assign_to` for targeting specific citizens
- [Inbox & Review](inbox-and-review.md) — the human side of reviewing agent-produced work
