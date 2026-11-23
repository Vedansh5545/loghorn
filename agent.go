package main

import "os"

// agentVars mark a process started by an AI coding agent: Claude Code sets
// CLAUDECODE, Codex sets CODEX_SANDBOX (and CODEX_SANDBOX_NETWORK_DISABLED) on
// the commands it runs, and AI_AGENT is the cross-tool convention.
var agentVars = []string{"CLAUDECODE", "AI_AGENT", "CODEX_SANDBOX", "CODEX_SANDBOX_NETWORK_DISABLED"}

// allowAgentVar lets an agent start serve anyway.
const allowAgentVar = "LOGHORN_ALLOW_AGENT"

// agentEnv names the set variable that shows loghorn was started by an AI
// coding agent, or "" when none is set or LOGHORN_ALLOW_AGENT is. serve and
// run -serve hold the data folder, socket and ports for as long as they run;
// an agent's shell would leave them held (or wait forever), so they are kept
// for people. An agent's run still records, through a running serve or
// serve-down.
func agentEnv(getenv func(string) string) string {
	if getenv(allowAgentVar) != "" {
		return ""
	}
	for _, v := range agentVars {
		if getenv(v) != "" {
			return v
		}
	}
	return ""
}

func runningUnderAgent() string { return agentEnv(os.Getenv) }
