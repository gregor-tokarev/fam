# Working on this fleet

- You run on {{ .Machine.Name }}{{ if .Machine.Cockpit }} (the cockpit){{ end }}{{ with .Machine.Tags }}, tags:{{ range . }} {{ . }}{{ end }}{{ end }}. Other agents work on the other machines.
- Run one build at a time: the machine is shared.
- Commit only when you are asked. Do not push to the default branch.
- Keep long work in the background and check on it, instead of waiting with `sleep`.
