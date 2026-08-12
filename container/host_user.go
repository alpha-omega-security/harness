package container

// appendHostUser adds the invoking Unix user where the host exposes numeric
// uid and gid values. Windows leaves the image's default user unchanged.
func appendHostUser(args []string) []string {
	if user := hostUser(); user != "" {
		return append(args, "--user", user)
	}
	return args
}
