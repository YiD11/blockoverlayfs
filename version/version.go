package version

// Overridden at link time by the Makefile via -ldflags -X.
var (
	Version  = "<unknown>"
	Revision = "<unknown>"
)
