// Package procgroup ends a command together with everything it started.
//
// exec.CommandContext kills only the process it started when its context
// ends. A shell, a build tool, or any command that forks leaves its children
// running, still holding the run's pipes and its CPU, after the run has been
// reported as timed out. Configure starts the command as the leader of a
// process group of its own and makes cancellation signal the whole group:
// SIGTERM first, then SIGKILL once a grace period has passed, whether or not
// the leader has already gone.
//
// Process groups are a Unix concept. On other platforms Configure leaves the
// command as exec configures it, which ends the started process only.
package procgroup
