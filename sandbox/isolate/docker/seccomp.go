package docker

import _ "embed"

// seccompProfile is the seccomp profile every container starts with:
// Docker's default profile (moby v28.0.0, profiles/seccomp/default.json)
// with every name in deniedSyscalls removed from every allow rule. The
// default action fails anything not allowed with EPERM, so a removed
// syscall fails with EPERM whatever capabilities the container has.
//
// It is passed as --security-opt seccomp=/dev/stdin with the profile on
// the CLI's standard input: the CLI reads the file itself and sends its
// contents to the daemon, so no copy on disk can be swapped in between.
//
//go:embed seccomp.json
var seccompProfile []byte

// deniedSyscalls are the syscalls the profile denies beyond Docker's
// default. Most of them Docker allows only with a capability the driver
// drops; removing them keeps them denied if one is ever added back.
// ptrace, process_vm_readv/writev and name_to_handle_at Docker allows
// without any capability. personality keeps Docker's own argument filter.
var deniedSyscalls = []string{
	// Inspecting or controlling other processes.
	"kcmp", "process_vm_readv", "process_vm_writev", "ptrace",
	// Mounts, namespaces and the root, old and new mount APIs alike.
	"fsconfig", "fsmount", "fsopen", "fspick", "mount", "mount_setattr",
	"move_mount", "open_tree", "pivot_root", "setns", "umount", "umount2",
	"unshare",
	// Kernel attack surface a workload has no use for.
	"bpf", "perf_event_open", "userfaultfd",
	"add_key", "keyctl", "request_key",
	// File handles, which bypass path-based confinement.
	"name_to_handle_at", "open_by_handle_at",
	// The machine itself.
	"acct", "delete_module", "finit_module", "init_module",
	"kexec_file_load", "kexec_load", "reboot", "swapoff", "swapon",
}
