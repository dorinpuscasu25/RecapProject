// Command openapi-gen writes api/openapi.yaml from internal/apispec.
package main

import (
	"fmt"
	"os"

	"github.com/dorinpuscasu25/recapproject/internal/apispec"
)

func main() {
	out := "api/openapi.yaml"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	b, err := apispec.YAML()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// #nosec G306 -- generated spec is a public repo file
	if err := os.WriteFile(out, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes)\n", out, len(b))
}
