// Code generated from Pkl module `fam.Bundle`. DO NOT EDIT.
package bundle

// A skill folder in a public GitHub repository. On each run, the cockpit downloads the repository
// archive once (`codeload.github.com`) and copies the folder to the machines.
type GitHubSkill struct {
	// The repository, `owner/name`.
	Repo string `pkl:"repo"`

	// The skill folder in the repository, for example `"skills/agent-browser"`.
	Path string `pkl:"path"`

	// A tag, branch, or commit. When it is `null`, FAM uses the tag of the latest release, so that
	// the skill matches the latest version of the tool.
	Ref *string `pkl:"ref"`
}
