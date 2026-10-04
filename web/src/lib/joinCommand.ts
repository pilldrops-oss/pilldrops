// The orchestrator has no configured public URL to hand back (see
// handleMintJoinToken, internal/orchestrator/join_handlers.go): it builds the
// join command from the request's own Host header, which is the browser's
// address right now — correct when reached directly, wrong behind a tunnel,
// reverse proxy, or private network the new host cannot see. Pulling it out
// lets the UI label it clearly instead of burying it in the command string.
export function orchestratorUrlFrom(bootstrapCmd: string): string | null {
  const m = bootstrapCmd.match(/ORCHESTRATOR_URL=(\S+)/)
  return m ? m[1] : null
}
