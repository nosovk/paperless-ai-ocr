package buildinfo

import "testing"

func TestDevelopmentMetadataHasUsefulPlaceholders(t *testing.T) {
	t.Parallel()

	metadata := Current()
	if metadata.Version == "" {
		t.Error("Version must not be empty")
	}
	if metadata.Revision == "" {
		t.Error("Revision must not be empty")
	}
	if metadata.BuildTime == "" {
		t.Error("BuildTime must not be empty")
	}
}
