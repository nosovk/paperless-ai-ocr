package main

import (
	"fmt"
	"os"

	"github.com/nosovk/paperless-ai-ocr/internal/buildinfo"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		metadata := buildinfo.Current()
		fmt.Printf(
			"paperless-ai-ocr version=%s revision=%s build_time=%s\n",
			metadata.Version,
			metadata.Revision,
			metadata.BuildTime,
		)
		return
	}

	fmt.Fprintln(os.Stderr, "paperless-ai-ocr: service is not yet configured")
	os.Exit(1)
}
