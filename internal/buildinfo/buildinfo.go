package buildinfo

var (
	version   = "development"
	revision  = "unknown"
	buildTime = "unknown"
)

// Metadata identifies the source and build represented by a binary.
type Metadata struct {
	Version   string
	Revision  string
	BuildTime string
}

// Current returns the metadata embedded in the current binary.
func Current() Metadata {
	return Metadata{
		Version:   version,
		Revision:  revision,
		BuildTime: buildTime,
	}
}
