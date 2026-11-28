package daemon

import (
	"os"

	"github.com/gregor-tokarev/fam/internal/transport"
)

// RPC protocol messages between the short-lived `fam` CLI and the background `famd` daemon over a
// local Unix domain socket (newline-delimited JSON).
type rpcReq struct {
	Op string `json:"op"`

	// Target session identity
	Machine   string `json:"machine,omitempty"`
	Target    string `json:"target,omitempty"`
	SSHConfig string `json:"ssh_config,omitempty"`
	OS        string `json:"os,omitempty"`

	// Bootstrap / Speculative checks
	FactsScript      string              `json:"facts_script,omitempty"`
	SecretsRel       string              `json:"secrets_rel,omitempty"`
	ExpectedFactsSHA string              `json:"expected_facts_sha,omitempty"`
	SpecReqs         []transport.SpecReq `json:"spec_reqs,omitempty"`

	// File / Skill / Exec operations
	Path        string        `json:"path,omitempty"`
	Dir         string        `json:"dir,omitempty"`
	Marker      string        `json:"marker,omitempty"`
	Data        []byte        `json:"data,omitempty"`
	Mode        os.FileMode   `json:"mode,omitempty"`
	Sudo        bool          `json:"sudo,omitempty"`
	Script      string        `json:"script,omitempty"`
	Stdin       []byte        `json:"stdin,omitempty"`
	LoginShell  bool          `json:"login_shell,omitempty"`
	Interactive bool          `json:"interactive,omitempty"`
	TimeoutMS   int64         `json:"timeout_ms,omitempty"`
	Label       string        `json:"label,omitempty"`
	Secret      bool          `json:"secret,omitempty"`
	IfNeeded    bool          `json:"if_needed,omitempty"`
	Ops         []transport.ApplyOp `json:"ops,omitempty"`
}

type rpcResp struct {
	OK           bool                          `json:"ok,omitempty"`
	Err          string                        `json:"err,omitempty"`
	ConnErr      bool                          `json:"conn_err,omitempty"`
	ConnStderr   string                        `json:"conn_stderr,omitempty"`
	SkillFallbk  bool                          `json:"skill_fallback,omitempty"`
	SupportsSeq  bool                          `json:"supports_seq,omitempty"`
	MaxConcur    int                           `json:"max_concur,omitempty"`
	SHA256Cmd    string                        `json:"sha256_cmd,omitempty"`

	// Bootstrap response
	FactsRes      transport.Result              `json:"facts_res,omitempty"`
	SecretsData   []byte                        `json:"secrets_data,omitempty"`
	SecretsExists bool                          `json:"secrets_exists,omitempty"`
	SpecHit       bool                          `json:"spec_hit,omitempty"`
	SpecResults   map[string]transport.WireResp `json:"spec_results,omitempty"`

	// Run response
	Result transport.Result `json:"result,omitempty"`

	// File / Skill responses
	FileInfo   transport.FileInfo   `json:"file_info,omitempty"`
	Data       []byte               `json:"data,omitempty"`
	Exists     bool                 `json:"exists,omitempty"`
	SkillState transport.SkillState `json:"skill_state,omitempty"`

	// Streaming ApplySeq frame
	Item *transport.ApplyOpResult `json:"item,omitempty"`
	Done bool                     `json:"done,omitempty"`
}
