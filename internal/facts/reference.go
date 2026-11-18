package facts

// Reference supplies stable Linux inputs for documentation and static evaluation.
func Reference() Facts {
	return Facts{OS: "linux", Distro: "ubuntu", DistroVersion: "26.04", Arch: "amd64", Libc: "glibc", CPUs: 4, Home: "/home/fam", User: "fam", Hostname: "fam-docs", Init: "systemd", Shell: "/bin/bash", Machine: "docs", Tags: []string{"worker"}}
}
