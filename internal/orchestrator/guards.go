package orchestrator

import "errors"

// Boot-time refusals, pure functions of the configuration so both branches are
// testable without a process that exits. main() treats a non-nil error as fatal.

// NodeKeySecretGuardError refuses a clustered control plane with no
// NODE_KEY_SECRET: deriving node credentials from a secret every node holds lets
// one compromised node mint every other node's. devInsecure is the local-dev escape hatch.
func NodeKeySecretGuardError(clustered, devInsecure bool, nodeKeySecret string) error {
	if !clustered || devInsecure || nodeKeySecret != "" {
		return nil
	}
	return errors.New("NODE_KEY_SECRET is not set — refusing to start a control plane that would " +
		"derive every node's NODE_API_KEY and heartbeat token from ORCHESTRATOR_TOKEN, which every " +
		"node already holds. In that mode one compromised node can mint every other node's " +
		"credentials, heartbeat as them, and be handed their sandboxes. " +
		"Generate one with `openssl rand -hex 32`, set NODE_KEY_SECRET on the control plane ONLY " +
		"(never on a node), then re-join each node so it picks up its newly derived NODE_API_KEY — " +
		"see docs/deployment.md, \"Migrating an existing fleet onto NODE_KEY_SECRET\". " +
		"Set DEV_INSECURE=1 for local development only")
}

// OrgAssertKeyGuardError refuses a clustered control plane with no
// ORG_ASSERT_KEY. The HMAC fallback cannot verify at a node, whose
// ORCHESTRATOR_TOKEN is its own derived heartbeat token, so every override would 403.
func OrgAssertKeyGuardError(clustered, devInsecure bool, orgAssertKey string) error {
	if !clustered || devInsecure || orgAssertKey != "" {
		return nil
	}
	return errors.New("ORG_ASSERT_KEY is not set — refusing to start a clustered control plane " +
		"that cannot sign org overrides. The legacy HMAC fallback signs with ORCHESTRATOR_TOKEN, " +
		"and since per-node credentials landed a node's ORCHESTRATOR_TOKEN is its own derived " +
		"heartbeat token, not the fleet secret — so the signature can never verify and every " +
		"X-Org-Override answers 403. Generate one with `openssl rand -hex 32`, set it here, and " +
		"give nodes its public half. Set DEV_INSECURE=1 for a single-tenant dev box only")
}
