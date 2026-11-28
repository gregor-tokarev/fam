package daemon

import "github.com/gregor-tokarev/fam/internal/transport"

// Minimal local Unix-socket relay protocol between `fam` and the short-lived `fam internal daemon`.
// The daemon pools SSH ControlMaster + persistent wire sessions and relays raw WireReq/WireResp
// frames without mirroring Transport methods or holding per-client spec caches.
type rpcReq struct {
	Op string `json:"op"` // ping, lease, stop, bootstrap, wire, wire_stream, refresh, reconnect

	Machine     string `json:"machine,omitempty"`
	Target      string `json:"target,omitempty"`
	SSHConfig   string `json:"ssh_config,omitempty"`
	SessionKey  string `json:"session_key,omitempty"`
	ControlSock string `json:"control_sock,omitempty"`

	// Bootstrap fields
	FactsScript      string              `json:"facts_script,omitempty"`
	SecretsRel       string              `json:"secrets_rel,omitempty"`
	ExpectedFactsSHA string              `json:"expected_facts_sha,omitempty"`
	SpecReqs         []transport.SpecReq `json:"spec_reqs,omitempty"`

	// Raw wire relay field
	Wire *transport.WireReq `json:"wire,omitempty"`
}

type rpcResp struct {
	OK          bool   `json:"ok,omitempty"`
	Err         string `json:"err,omitempty"`
	ConnErr     bool   `json:"conn_err,omitempty"`
	ConnStderr  string `json:"conn_stderr,omitempty"`
	IdentityErr bool   `json:"identity_err,omitempty"`
	HasWire     bool   `json:"has_wire,omitempty"`
	SessionKey  string `json:"session_key,omitempty"`
	ControlSock string `json:"control_sock,omitempty"`

	// Bootstrap response
	FactsRes      transport.Result              `json:"facts_res,omitempty"`
	SecretsData   []byte                        `json:"secrets_data,omitempty"`
	SecretsExists bool                          `json:"secrets_exists,omitempty"`
	SpecHit       bool                          `json:"spec_hit,omitempty"`
	SpecResults   map[string]transport.WireResp `json:"spec_results,omitempty"`

	// Raw wire relay response (single or streamed frame)
	Wire *transport.WireResp `json:"wire,omitempty"`
	Done bool                `json:"done,omitempty"`
}
